package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/gomarkdown/markdown"
	"github.com/gomarkdown/markdown/ast"
	"github.com/gomarkdown/markdown/parser"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclparse"
	"mvdan.cc/xurls/v2"
)

// Validator is an interface for all validators
type Validator interface {
	Validate() []error
}

// MarkdownValidator orchestrates all validations
type MarkdownValidator struct {
	readmePath string
	data       string
	tfDocs     string
	validators []Validator
}

// NewMarkdownValidator creates a new MarkdownValidator
func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	absReadmePath, err := filepath.Abs(readmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %v", err)
	}

	dataBytes, err := os.ReadFile(absReadmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}
	data := string(dataBytes)

	tfDocs, err := extractTFDocs(data)
	if err != nil {
		return nil, fmt.Errorf("failed to extract TF Docs: %v", err)
	}

	mv := &MarkdownValidator{
		readmePath: absReadmePath,
		data:       data,
		tfDocs:     tfDocs,
	}

	// Initialize validators
	mv.validators = []Validator{
		NewSectionValidator(data),
		NewTFDocsSectionValidator(tfDocs),
		NewFileValidator(absReadmePath),
		NewURLValidator(data),
		NewTerraformDefinitionValidator(tfDocs),
		NewItemValidator(tfDocs, "Resources", "resource", "Resources", "main.tf"),
		NewItemValidator(tfDocs, "Inputs", "variable", "Inputs", "variables.tf"),
		NewItemValidator(tfDocs, "Outputs", "output", "Outputs", "outputs.tf"),
	}

	return mv, nil
}

// Validate runs all registered validators
func (mv *MarkdownValidator) Validate() []error {
	var allErrors []error
	for _, validator := range mv.validators {
		errs := validator.Validate()
		for _, err := range errs {
			log.Printf("Validation error: %v", err) // Debugging statement
			allErrors = append(allErrors, err)
		}
	}
	return allErrors
}

// extractTFDocs extracts the content between <!-- BEGIN_TF_DOCS --> and <!-- END_TF_DOCS -->
func extractTFDocs(data string) (string, error) {
	beginMarker := "<!-- BEGIN_TF_DOCS -->"
	endMarker := "<!-- END_TF_DOCS -->"

	beginIdx := strings.Index(data, beginMarker)
	if beginIdx == -1 {
		return "", fmt.Errorf("begin marker %s not found", beginMarker)
	}
	beginIdx += len(beginMarker)

	endIdx := strings.Index(data, endMarker)
	if endIdx == -1 {
		return "", fmt.Errorf("end marker %s not found", endMarker)
	}

	tfDocs := strings.TrimSpace(data[beginIdx:endIdx])
	log.Printf("Extracted TF Docs:\n%s", tfDocs) // Debugging statement
	return tfDocs, nil
}

// Section represents a section in the markdown file
type Section struct {
	Header string
}

// SectionValidator validates markdown sections outside TF Docs
type SectionValidator struct {
	data     string
	sections []Section
	rootNode ast.Node
}

// NewSectionValidator creates a new SectionValidator for sections outside TF Docs
func NewSectionValidator(data string) *SectionValidator {
	sections := []Section{
		{Header: "Goals"},
		{Header: "Non-Goals"},
		{Header: "Features"},
		{Header: "Testing"},
		{Header: "Notes"},
		{Header: "Contributing"},
		{Header: "Authors"},
		{Header: "License"},
	}

	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	return &SectionValidator{
		data:     data,
		sections: sections,
		rootNode: rootNode,
	}
}

// Validate validates the sections outside TF Docs in the markdown
func (sv *SectionValidator) Validate() []error {
	var allErrors []error
	for _, section := range sv.sections {
		allErrors = append(allErrors, section.validate(sv.rootNode)...)
	}
	return allErrors
}

// validate checks if a section is present and has content
func (s Section) validate(rootNode ast.Node) []error {
	var errors []error
	found := false

	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, s.Header) || strings.EqualFold(text, s.Header+"s") {
				found = true

				// Check for content after the header
				nextNode := getNextSibling(node)
				if nextNode == nil || isNextHeader(nextNode) {
					errors = append(errors, fmt.Errorf("empty section: %s", s.Header))
				}

				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})

	if !found {
		errors = append(errors, fmt.Errorf("section not found: %s", s.Header))
	}

	return errors
}

// TFDocsSectionValidator validates sections within TF Docs
type TFDocsSectionValidator struct {
	data     string
	sections []Section
	rootNode ast.Node
}

// NewTFDocsSectionValidator creates a new SectionValidator for sections within TF Docs
func NewTFDocsSectionValidator(tfDocs string) *TFDocsSectionValidator {
	sections := []Section{
		{Header: "Requirements"},
		{Header: "Providers"},
		{Header: "Modules"},
		{Header: "Resources"},
		{Header: "Required Inputs"},
		{Header: "Optional Inputs"},
		{Header: "Outputs"},
	}

	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(tfDocs), p)

	return &TFDocsSectionValidator{
		data:     tfDocs,
		sections: sections,
		rootNode: rootNode,
	}
}

// Validate validates the sections within TF Docs in the markdown
func (sv *TFDocsSectionValidator) Validate() []error {
	var allErrors []error
	for _, section := range sv.sections {
		allErrors = append(allErrors, section.validate(sv.rootNode)...)
	}
	return allErrors
}

// FileValidator validates the presence of required files
type FileValidator struct {
	files []string
}

// NewFileValidator creates a new FileValidator
func NewFileValidator(readmePath string) *FileValidator {
	rootDir := filepath.Dir(readmePath)
	files := []string{
		readmePath,
		filepath.Join(rootDir, "CONTRIBUTING.md"),
		filepath.Join(rootDir, "CODE_OF_CONDUCT.md"),
		filepath.Join(rootDir, "SECURITY.md"),
		filepath.Join(rootDir, "LICENSE"),
		filepath.Join(rootDir, "outputs.tf"),
		filepath.Join(rootDir, "variables.tf"),
		filepath.Join(rootDir, "terraform.tf"),
		filepath.Join(rootDir, "Makefile"),
	}
	return &FileValidator{
		files: files,
	}
}

// Validate checks if required files exist and are not empty
func (fv *FileValidator) Validate() []error {
	var allErrors []error
	for _, filePath := range fv.files {
		allErrors = append(allErrors, validateFile(filePath)...)
	}
	return allErrors
}

// URLValidator validates URLs in the markdown
type URLValidator struct {
	data string
}

// NewURLValidator creates a new URLValidator
func NewURLValidator(data string) *URLValidator {
	return &URLValidator{data: data}
}

// Validate checks all URLs in the markdown for accessibility
func (uv *URLValidator) Validate() []error {
	return validateURLs(uv.data)
}

// TerraformDefinitionValidator validates Terraform definitions within TF Docs
type TerraformDefinitionValidator struct {
	data string
}

// NewTerraformDefinitionValidator creates a new TerraformDefinitionValidator
func NewTerraformDefinitionValidator(tfDocs string) *TerraformDefinitionValidator {
	return &TerraformDefinitionValidator{data: tfDocs}
}

// ResourceItem represents a resource or data source in the markdown
type ResourceItem struct {
	Type        string
	Description string
}

// Validate compares Terraform resources and data sources with those documented in the markdown
func (tdv *TerraformDefinitionValidator) Validate() []error {
	tfResources, tfDataSources, err := extractTerraformResources()
	if err != nil {
		return []error{err}
	}

	readmeResources, err := extractMarkdownSectionItems(tdv.data, "Resources")
	if err != nil {
		return []error{err}
	}

	log.Printf("Extracted Resources from Markdown: %+v", readmeResources) // Debugging statement

	// Classify resources and data sources based on description
	var readmeDataSources []string
	var readmeResourceTypes []string
	for _, res := range readmeResources {
		if strings.EqualFold(res.Description, "resource") {
			readmeResourceTypes = append(readmeResourceTypes, res.Type)
		} else if strings.EqualFold(res.Description, "data source") {
			readmeDataSources = append(readmeDataSources, res.Type)
		}
	}

	log.Printf("Classified Resource Types: %+v", readmeResourceTypes)      // Debugging statement
	log.Printf("Classified Data Sources: %+v", readmeDataSources)        // Debugging statement

	var errors []error
	errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResourceTypes, "Resources")...)
	errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)

	return errors
}

// ItemValidator validates items in Terraform and markdown within TF Docs
type ItemValidator struct {
	data      string
	itemType  string
	blockType string
	section   string
	fileName  string
}

// NewItemValidator creates a new ItemValidator
func NewItemValidator(tfDocs, itemType, blockType, section, fileName string) *ItemValidator {
	return &ItemValidator{
		data:      tfDocs,
		itemType:  itemType,
		blockType: blockType,
		section:   section,
		fileName:  fileName,
	}
}

// Validate compares Terraform items with those documented in the markdown
func (iv *ItemValidator) Validate() []error {
	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return []error{fmt.Errorf("failed to get current working directory: %v", err)}
		}
	}
	filePath := filepath.Join(workspace, "caller", iv.fileName)
	tfItems, err := extractTerraformItems(filePath, iv.blockType)
	if err != nil {
		return []error{err}
	}

	var mdItems []string
	// Attempt to extract from the main section
	items, err := extractMarkdownSectionItems(iv.data, iv.section)
	if err != nil {
		// If the main section isn't found, try aggregating from subsections
		if iv.section == "Inputs" {
			requiredInputs, err1 := extractMarkdownSectionItems(iv.data, "Required Inputs")
			optionalInputs, err2 := extractMarkdownSectionItems(iv.data, "Optional Inputs")
			if err1 == nil {
				for _, item := range requiredInputs {
					mdItems = append(mdItems, item.Type)
				}
			}
			if err2 == nil {
				for _, item := range optionalInputs {
					mdItems = append(mdItems, item.Type)
				}
			}
			if len(mdItems) == 0 {
				return []error{fmt.Errorf("no inputs found in markdown")}
			}
		} else {
			return []error{err}
		}
	} else {
		for _, item := range items {
			mdItems = append(mdItems, item.Type)
		}
	}

	return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
}

// Helper functions

func isNextHeader(node ast.Node) bool {
	_, ok := node.(*ast.Heading)
	return ok
}

func getNextSibling(node ast.Node) ast.Node {
	parent := node.GetParent()
	if parent == nil {
		return nil
	}
	children := parent.GetChildren()
	for i, n := range children {
		if n == node && i+1 < len(children) {
			return children[i+1]
		}
	}
	return nil
}

func extractText(node ast.Node) string {
	var sb strings.Builder
	ast.WalkFunc(node, func(n ast.Node, entering bool) ast.WalkStatus {
		if entering {
			switch tn := n.(type) {
			case *ast.Text:
				sb.Write(tn.Literal)
			case *ast.Code:
				sb.Write(tn.Literal)
			}
		}
		return ast.GoToNext
	})
	return sb.String()
}

func extractTextFromNodes(nodes []ast.Node) string {
	var sb strings.Builder
	for _, node := range nodes {
		sb.WriteString(extractText(node))
	}
	return sb.String()
}

func validateFile(filePath string) []error {
	var errors []error
	fileInfo, err := os.Stat(filePath)
	baseName := filepath.Base(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			errors = append(errors, fmt.Errorf("file does not exist: %s", baseName))
		} else {
			errors = append(errors, fmt.Errorf("error accessing file: %s: %v", baseName, err))
		}
		return errors
	}

	if fileInfo.Size() == 0 {
		errors = append(errors, fmt.Errorf("file is empty: %s", baseName))
	}

	return errors
}

func validateURLs(data string) []error {
	rxStrict := xurls.Strict()
	urls := rxStrict.FindAllString(data, -1)

	var wg sync.WaitGroup
	errChan := make(chan error, len(urls))

	for _, u := range urls {
		if strings.Contains(u, "registry.terraform.io/providers/") {
			continue
		}

		wg.Add(1)
		go func(url string) {
			defer wg.Done()
			if err := validateSingleURL(url); err != nil {
				errChan <- err
			}
		}(u)
	}

	wg.Wait()
	close(errChan)

	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	return errors
}

func validateSingleURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("error accessing URL: %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("URL returned non-OK status: %s: Status: %d", url, resp.StatusCode)
	}

	return nil
}

// extractMarkdownSectionItems extracts items from a specified section in the markdown
func extractMarkdownSectionItems(data, sectionName string) ([]ResourceItem, error) {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	var items []ResourceItem
	var inTargetSection bool

	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, sectionName) || strings.EqualFold(text, sectionName+"s") {
				inTargetSection = true
				return ast.GoToNext
			}
			if inTargetSection {
				return ast.Terminate
			}
		}

		if inTargetSection && entering {
			if list, ok := node.(*ast.List); ok {
				items = append(items, extractItemsFromList(list)...)
			}
		}
		return ast.GoToNext
	})

	if len(items) == 0 {
		return nil, fmt.Errorf("%s section not found or empty", sectionName)
	}

	return items, nil
}

func extractItemsFromList(list *ast.List) []ResourceItem {
	var items []ResourceItem
	for _, item := range list.Children {
		if listItem, ok := item.(*ast.ListItem); ok {
			for _, child := range listItem.GetChildren() {
				if paragraph, ok := child.(*ast.Paragraph); ok {
					text := extractTextFromNodes(paragraph.GetChildren())
					item := extractResourceItemFromText(text)
					if item.Type != "" {
						items = append(items, item)
					}
					break
				}
			}
		}
	}
	return items
}

func extractResourceItemFromText(text string) ResourceItem {
	// Regex to capture [resource.name](url) (type)
	re := regexp.MustCompile(`\[[^\]]+\]\([^\)]+\)\s*\(([^)]+)\)`)
	match := re.FindStringSubmatch(text)
	if len(match) < 2 {
		return ResourceItem{}
	}
	description := strings.TrimSpace(match[1])

	// Extract the resource type from the markdown list item
	// Example: [azurerm_management_lock.lock](...) (resource)
	// We need to extract "azurerm_management_lock" for resources and "azurerm_resource_group" for data sources

	// Extract the text inside the first square brackets
	reType := regexp.MustCompile(`\[(.+?)\]`)
	typeMatch := reType.FindStringSubmatch(text)
	if len(typeMatch) < 2 {
		return ResourceItem{}
	}
	fullType := typeMatch[1]

	// Extract the resource type by splitting at the first dot
	parts := strings.SplitN(fullType, ".", 2)
	if len(parts) < 1 {
		return ResourceItem{}
	}
	resourceType := strings.TrimSpace(parts[0])

	return ResourceItem{
		Type:        resourceType,
		Description: description,
	}
}

func extractTerraformResources() ([]string, []string, error) {
	var resources []string
	var dataSources []string

	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, nil, fmt.Errorf("failed to get current working directory: %v", err)
		}
	}
	mainPath := filepath.Join(workspace, "caller", "main.tf")
	specificResources, specificDataSources, err := extractFromFilePath(mainPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	resources = append(resources, specificResources...)
	dataSources = append(dataSources, specificDataSources...)

	modulesPath := filepath.Join(workspace, "caller", "modules")
	modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
	if err != nil {
		return nil, nil, err
	}
	resources = append(resources, modulesResources...)
	dataSources = append(dataSources, modulesDataSources...)

	return resources, dataSources, nil
}

func extractRecursively(dirPath string) ([]string, []string, error) {
	var resources []string
	var dataSources []string
	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		return resources, dataSources, nil
	} else if err != nil {
		return nil, nil, err
	}
	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() && filepath.Ext(path) == ".tf" {
			fileResources, fileDataSources, err := extractFromFilePath(path)
			if err != nil {
				return err
			}
			resources = append(resources, fileResources...)
			dataSources = append(dataSources, fileDataSources...)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return resources, dataSources, nil
}

func extractFromFilePath(filePath string) ([]string, []string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
	}

	parser := hclparse.NewParser()
	file, parseDiags := parser.ParseHCL(content, filePath)
	if parseDiags.HasErrors() {
		return nil, nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
	}

	var resources []string
	var dataSources []string
	body := file.Body

	// Initialize diagnostics variable
	var diags hcl.Diagnostics

	// Use PartialContent to allow unknown blocks
	hclContent, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "resource", LabelNames: []string{"type", "name"}},
			{Type: "data", LabelNames: []string{"type", "name"}},
		},
	})

	// Append diagnostics
	diags = append(diags, contentDiags...)

	// Filter out diagnostics related to unsupported block types
	diags = filterUnsupportedBlockDiagnostics(diags)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	}

	if hclContent == nil {
		// No relevant blocks found
		return resources, dataSources, nil
	}

	for _, block := range hclContent.Blocks {
		if len(block.Labels) >= 2 {
			resourceType := strings.TrimSpace(block.Labels[0])
			if block.Type == "resource" {
				resources = append(resources, resourceType)
			} else if block.Type == "data" {
				dataSources = append(dataSources, resourceType)
			}
		}
	}

	return resources, dataSources, nil
}

func filterUnsupportedBlockDiagnostics(diags hcl.Diagnostics) hcl.Diagnostics {
	var filteredDiags hcl.Diagnostics
	for _, diag := range diags {
		if diag.Severity == hcl.DiagError && strings.Contains(diag.Summary, "Unsupported block type") {
			continue
		}
		filteredDiags = append(filteredDiags, diag)
	}
	return filteredDiags
}

func extractTerraformItems(filePath string, blockType string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
	}

	parser := hclparse.NewParser()
	file, parseDiags := parser.ParseHCL(content, filePath)
	if parseDiags.HasErrors() {
		return nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
	}

	var items []string
	body := file.Body

	// Initialize diagnostics variable
	var diags hcl.Diagnostics

	var schema hcl.BodySchema

	switch blockType {
	case "resource", "data":
		schema = hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: blockType, LabelNames: []string{"type", "name"}},
			},
		}
	case "variable", "output":
		schema = hcl.BodySchema{
			Blocks: []hcl.BlockHeaderSchema{
				{Type: blockType, LabelNames: []string{"name"}},
			},
		}
	default:
		return nil, fmt.Errorf("unsupported block type: %s", blockType)
	}

	// Use PartialContent to extract only the specified block type
	hclContent, _, contentDiags := body.PartialContent(&schema)

	// Append diagnostics
	diags = append(diags, contentDiags...)

	// Filter out diagnostics related to unsupported block types
	diags = filterUnsupportedBlockDiagnostics(diags)
	if diags.HasErrors() {
		return nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	}

	if hclContent == nil {
		// No relevant blocks found
		return items, nil
	}

	for _, block := range hclContent.Blocks {
		switch blockType {
		case "resource", "data":
			if len(block.Labels) >= 2 {
				// For resource and data, second label is name
				itemName := strings.TrimSpace(block.Labels[1])
				items = append(items, itemName)
			}
		case "variable", "output":
			if len(block.Labels) >= 1 {
				itemName := strings.TrimSpace(block.Labels[0])
				items = append(items, itemName)
			}
		}
	}

	return items, nil
}

func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
	var errors []error

	// Normalize case and create sets
	tfSet := make(map[string]struct{})
	for _, item := range tfItems {
		tfSet[strings.ToLower(item)] = struct{}{}
	}

	mdSet := make(map[string]struct{})
	for _, item := range mdItems {
		mdSet[strings.ToLower(item)] = struct{}{}
	}

	// Find items missing in markdown
	var missingInMarkdown []string
	for item := range tfSet {
		if _, found := mdSet[item]; !found {
			missingInMarkdown = append(missingInMarkdown, item)
		}
	}

	// Find items in markdown but missing in Terraform
	var missingInTerraform []string
	for item := range mdSet {
		if _, found := tfSet[item]; !found {
			missingInTerraform = append(missingInTerraform, item)
		}
	}

	if len(missingInMarkdown) > 0 {
		errors = append(errors, fmt.Errorf("%s missing in markdown: %s", itemType, strings.Join(missingInMarkdown, ", ")))
	}

	if len(missingInTerraform) > 0 {
		errors = append(errors, fmt.Errorf("%s listed in markdown but not found in Terraform: %s", itemType, strings.Join(missingInTerraform, ", ")))
	}

	return errors
}

// TestMarkdown is the main test function
func TestMarkdown(t *testing.T) {
	readmePath := "README.md"
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	validator, err := NewMarkdownValidator(readmePath)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	errors := validator.Validate()
	if len(errors) > 0 {
		for _, err := range errors {
			t.Errorf("Validation error: %v", err)
		}
		t.FailNow()
	}
}

//package main

//import (
	//"errors"
	//"fmt"
	//"net/http"
	//"os"
	//"path/filepath"
	//"strings"
	//"sync"
	//"testing"

	//"github.com/gomarkdown/markdown"
	//"github.com/gomarkdown/markdown/ast"
	//"github.com/gomarkdown/markdown/parser"
	//"github.com/hashicorp/hcl/v2"
	//"github.com/hashicorp/hcl/v2/hclparse"
	//"mvdan.cc/xurls/v2"
//)

//// Validator is an interface for all validators
//type Validator interface {
	//Validate() []error
//}

//// MarkdownValidator orchestrates all validations
//type MarkdownValidator struct {
	//readmePath string
	//data       string
	//validators []Validator
//}

//// NewMarkdownValidator creates a new MarkdownValidator
//func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
	//if envPath := os.Getenv("README_PATH"); envPath != "" {
		//readmePath = envPath
	//}

	//absReadmePath, err := filepath.Abs(readmePath)
	//if err != nil {
		//return nil, fmt.Errorf("failed to get absolute path: %v", err)
	//}

	//dataBytes, err := os.ReadFile(absReadmePath)
	//if err != nil {
		//return nil, fmt.Errorf("failed to read file: %v", err)
	//}
	//data := string(dataBytes)

	//mv := &MarkdownValidator{
		//readmePath: absReadmePath,
		//data:       data,
	//}

	//// Initialize validators
	//mv.validators = []Validator{
		//NewSectionValidator(data),
		//NewFileValidator(absReadmePath),
		//NewURLValidator(data),
		//NewTerraformDefinitionValidator(data),
		//NewItemValidator(data, "Variables", "variable", "Inputs", "variables.tf"),
		//NewItemValidator(data, "Outputs", "output", "Outputs", "outputs.tf"),
	//}

	//return mv, nil
//}

//// Validate runs all registered validators
//func (mv *MarkdownValidator) Validate() []error {
	//var allErrors []error
	//for _, validator := range mv.validators {
		//allErrors = append(allErrors, validator.Validate()...)
	//}
	//return allErrors
//}

//// Section represents a markdown section
//type Section struct {
	//Header  string
	//Columns []string
//}

//// SectionValidator validates markdown sections
//type SectionValidator struct {
	//data     string
	//sections []Section
	//rootNode ast.Node
//}

//// NewSectionValidator creates a new SectionValidator
//func NewSectionValidator(data string) *SectionValidator {
	//sections := []Section{
		//{Header: "Goals"},
		//{Header: "Resources", Columns: []string{"Name", "Type"}},
		//{Header: "Providers", Columns: []string{"Name", "Version"}},
		//{Header: "Requirements", Columns: []string{"Name", "Version"}},
		//{Header: "Inputs", Columns: []string{"Name", "Description", "Type", "Required"}},
		//{Header: "Outputs", Columns: []string{"Name", "Description"}},
		//{Header: "Features"},
		//{Header: "Testing"},
		//{Header: "Authors"},
		//{Header: "License"},
	//}

	//// Parse the markdown content into an AST
	//extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	//p := parser.NewWithExtensions(extensions)
	//rootNode := markdown.Parse([]byte(data), p)

	//return &SectionValidator{
		//data:     data,
		//sections: sections,
		//rootNode: rootNode,
	//}
//}

//// Validate validates the sections in the markdown
//func (sv *SectionValidator) Validate() []error {
	//var allErrors []error
	//for _, section := range sv.sections {
		//allErrors = append(allErrors, section.validate(sv.rootNode)...)
	//}
	//return allErrors
//}

//// validate checks if a section and its columns are correctly formatted
//func (s Section) validate(rootNode ast.Node) []error {
	//var errors []error
	//found := false

	//// Traverse the AST to find the section header
	//ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		//if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			//text := strings.TrimSpace(extractText(heading))
			//if strings.EqualFold(text, s.Header) || strings.EqualFold(text, s.Header+"s") {
				//found = true

				//if len(s.Columns) > 0 {
					//// Check for the table after the header
					//nextNode := getNextSibling(node)
					//if table, ok := nextNode.(*ast.Table); ok {
						//// Extract table headers
						//actualHeaders, err := extractTableHeaders(table)
						//if err != nil {
							//errors = append(errors, err)
						//} else if !equalSlices(actualHeaders, s.Columns) {
							//errors = append(errors, compareColumns(s.Header, s.Columns, actualHeaders))
						//}
					//} else {
						//errors = append(errors, formatError("missing table after header: %s", s.Header))
					//}
				//}
				//return ast.SkipChildren
			//}
		//}
		//return ast.GoToNext
	//})

	//if !found {
		//errors = append(errors, compareHeaders(s.Header, ""))
	//}

	//return errors
//}

//// getNextSibling returns the next sibling of a node
//func getNextSibling(node ast.Node) ast.Node {
	//parent := node.GetParent()
	//if parent == nil {
		//return nil
	//}
	//children := parent.GetChildren()
	//for i, n := range children {
		//if n == node && i+1 < len(children) {
			//return children[i+1]
		//}
	//}
	//return nil
//}

//// extractTableHeaders extracts headers from a markdown table
//func extractTableHeaders(table *ast.Table) ([]string, error) {
	//headers := []string{}

	//if len(table.GetChildren()) == 0 {
		//return nil, fmt.Errorf("table is empty")
	//}

	//// The first child should be TableHeader
	//if headerNode, ok := table.GetChildren()[0].(*ast.TableHeader); ok {
		//for _, rowNode := range headerNode.GetChildren() {
			//if row, ok := rowNode.(*ast.TableRow); ok {
				//for _, cellNode := range row.GetChildren() {
					//if cell, ok := cellNode.(*ast.TableCell); ok {
						//headerText := strings.TrimSpace(extractTextFromNodes(cell.GetChildren()))
						//headers = append(headers, headerText)
					//}
				//}
			//}
		//}
	//} else {
		//return nil, fmt.Errorf("table has no header row")
	//}

	//return headers, nil
//}

//// FileValidator validates the presence of required files
//type FileValidator struct {
	//files []string
//}

//func NewFileValidator(readmePath string) *FileValidator {
	//rootDir := filepath.Dir(readmePath)
	//files := []string{
		//readmePath,
		//filepath.Join(rootDir, "CONTRIBUTING.md"),
		//filepath.Join(rootDir, "CODE_OF_CONDUCT.md"),
		//filepath.Join(rootDir, "SECURITY.md"),
		//filepath.Join(rootDir, "LICENSE"),
		//filepath.Join(rootDir, "outputs.tf"),
		//filepath.Join(rootDir, "variables.tf"),
		//filepath.Join(rootDir, "terraform.tf"),
		//filepath.Join(rootDir, "Makefile"),
	//}
	//return &FileValidator{
		//files: files,
	//}
//}

//// Validate checks if required files exist and are not empty
//func (fv *FileValidator) Validate() []error {
	//var allErrors []error
	//for _, filePath := range fv.files {
		//allErrors = append(allErrors, validateFile(filePath)...)
	//}
	//return allErrors
//}

//// validateFile checks if a file exists and is not empty
//func validateFile(filePath string) []error {
	//var errors []error
	//fileInfo, err := os.Stat(filePath)
	//baseName := filepath.Base(filePath)
	//if err != nil {
		//if os.IsNotExist(err) {
			//errors = append(errors, formatError("file does not exist:\n  %s", baseName))
		//} else {
			//errors = append(errors, formatError("error accessing file:\n  %s\n  %v", baseName, err))
		//}
		//return errors
	//}

	//if fileInfo.Size() == 0 {
		//errors = append(errors, formatError("file is empty:\n  %s", baseName))
	//}

	//return errors
//}

//// URLValidator validates URLs in the markdown
//type URLValidator struct {
	//data string
//}

//// NewURLValidator creates a new URLValidator
//func NewURLValidator(data string) *URLValidator {
	//return &URLValidator{data: data}
//}

//// Validate checks all URLs in the markdown for accessibility
//func (uv *URLValidator) Validate() []error {
	//return validateURLs(uv.data)
//}

//// validateURLs checks if URLs in the data are accessible
//func validateURLs(data string) []error {
	//rxStrict := xurls.Strict()
	//urls := rxStrict.FindAllString(data, -1)

	//var wg sync.WaitGroup
	//errChan := make(chan error, len(urls))

	//for _, u := range urls {
		//if strings.Contains(u, "registry.terraform.io/providers/") {
			//continue
		//}

		//wg.Add(1)
		//go func(url string) {
			//defer wg.Done()
			//if err := validateSingleURL(url); err != nil {
				//errChan <- err
			//}
		//}(u)
	//}

	//wg.Wait()
	//close(errChan)

	//var errors []error
	//for err := range errChan {
		//errors = append(errors, err)
	//}

	//return errors
//}

//// validateSingleURL checks if a single URL is accessible
//func validateSingleURL(url string) error {
	//resp, err := http.Get(url)
	//if err != nil {
		//return formatError("error accessing URL:\n  %s\n  %v", url, err)
	//}
	//defer resp.Body.Close()

	//if resp.StatusCode != http.StatusOK {
		//return formatError("URL returned non-OK status:\n  %s\n  Status: %d", url, resp.StatusCode)
	//}

	//return nil
//}

//// TerraformDefinitionValidator validates Terraform definitions
//type TerraformDefinitionValidator struct {
	//data string
//}

//// NewTerraformDefinitionValidator creates a new TerraformDefinitionValidator
//func NewTerraformDefinitionValidator(data string) *TerraformDefinitionValidator {
	//return &TerraformDefinitionValidator{data: data}
//}

//// Validate compares Terraform resources with those documented in the markdown
//func (tdv *TerraformDefinitionValidator) Validate() []error {
	//tfResources, tfDataSources, err := extractTerraformResources()
	//if err != nil {
		//return []error{err}
	//}

	//readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
	//if err != nil {
		//return []error{err}
	//}

	//var errors []error
	//errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
	//errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)

	//return errors
//}

//// ItemValidator validates items in Terraform and markdown
//type ItemValidator struct {
	//data      string
	//itemType  string
	//blockType string
	//section   string
	//fileName  string
//}

//// NewItemValidator creates a new ItemValidator
//func NewItemValidator(data, itemType, blockType, section, fileName string) *ItemValidator {
	//return &ItemValidator{
		//data:      data,
		//itemType:  itemType,
		//blockType: blockType,
		//section:   section,
		//fileName:  fileName,
	//}
//}

//// Validate compares Terraform items with those documented in the markdown
//func (iv *ItemValidator) Validate() []error {
	//workspace := os.Getenv("GITHUB_WORKSPACE")
	//if workspace == "" {
		//var err error
		//workspace, err = os.Getwd()
		//if err != nil {
			//return []error{fmt.Errorf("failed to get current working directory: %v", err)}
		//}
	//}
	//filePath := filepath.Join(workspace, "caller", iv.fileName)
	//tfItems, err := extractTerraformItems(filePath, iv.blockType)
	//if err != nil {
		//return []error{err}
	//}

	//mdItems, err := extractMarkdownSectionItems(iv.data, iv.section)
	//if err != nil {
		//return []error{err}
	//}

	//return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
//}

//// Helper functions

//// compareHeaders compares expected and actual headers
//func compareHeaders(expected, actual string) error {
	//if expected != actual {
		//if actual == "" {
			//return formatError("incorrect header:\n  expected '%s', found 'not present'", expected)
		//}
		//return formatError("incorrect header:\n  expected '%s', found '%s'", expected, actual)
	//}
	//return nil
//}

//// formatError formats an error message
//func formatError(format string, args ...interface{}) error {
	//return fmt.Errorf(format, args...)
//}

//// equalSlices checks if two slices are equal
//func equalSlices(a, b []string) bool {
	//if len(a) != len(b) {
		//return false
	//}
	//for i, v := range a {
		//if v != b[i] {
			//return false
		//}
	//}
	//return true
//}

//// compareColumns compares expected and actual table columns
//func compareColumns(header string, expected, actual []string) error {
	//var mismatches []string
	//for i := 0; i < len(expected) || i < len(actual); i++ {
		//var exp, act string
		//if i < len(expected) {
			//exp = expected[i]
		//}
		//if i < len(actual) {
			//act = actual[i]
		//}
		//if exp != act {
			//mismatches = append(mismatches, fmt.Sprintf("expected '%s', found '%s'", exp, act))
		//}
	//}
	//return formatError("table under header: %s has incorrect column names:\n  %s", header, strings.Join(mismatches, "\n  "))
//}

//// findMissingItems finds items in a that are not in b
//func findMissingItems(a, b []string) []string {
	//bSet := make(map[string]struct{}, len(b))
	//for _, x := range b {
		//bSet[x] = struct{}{}
	//}
	//var missing []string
	//for _, x := range a {
		//if _, found := bSet[x]; !found {
			//missing = append(missing, x)
		//}
	//}
	//return missing
//}

//// compareTerraformAndMarkdown compares items in Terraform and markdown
//func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
	//var errors []error

	//missingInMarkdown := findMissingItems(tfItems, mdItems)
	//if len(missingInMarkdown) > 0 {
		//errors = append(errors, formatError("%s missing in markdown:\n  %s", itemType, strings.Join(missingInMarkdown, "\n  ")))
	//}

	//missingInTerraform := findMissingItems(mdItems, tfItems)
	//if len(missingInTerraform) > 0 {
		//errors = append(errors, formatError("%s in markdown but missing in Terraform:\n  %s", itemType, strings.Join(missingInTerraform, "\n  ")))
	//}

	//return errors
//}

//// extractTerraformItems extracts item names from a Terraform file given the block type
//func extractTerraformItems(filePath string, blockType string) ([]string, error) {
	//content, err := os.ReadFile(filePath)
	//if err != nil {
		//return nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
	//}

	//parser := hclparse.NewParser()
	//file, parseDiags := parser.ParseHCL(content, filePath)
	//if parseDiags.HasErrors() {
		//return nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
	//}

	//var items []string
	//body := file.Body

	//// Initialize diagnostics variable
	//var diags hcl.Diagnostics

	//// Use PartialContent to extract only the specified block type
	//hclContent, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		//Blocks: []hcl.BlockHeaderSchema{
			//{Type: blockType, LabelNames: []string{"name"}},
		//},
	//})

	//// Append diagnostics
	//diags = append(diags, contentDiags...)

	//// Filter out diagnostics related to unsupported block types
	//diags = filterUnsupportedBlockDiagnostics(diags)
	//if diags.HasErrors() {
		//return nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	//}

	//if hclContent == nil {
		//// No relevant blocks found
		//return items, nil
	//}

	//for _, block := range hclContent.Blocks {
		//if len(block.Labels) > 0 {
			//itemName := strings.TrimSpace(block.Labels[0])
			//items = append(items, itemName)
		//}
	//}

	//return items, nil
//}

//// filterUnsupportedBlockDiagnostics filters out diagnostics related to unsupported block types
//func filterUnsupportedBlockDiagnostics(diags hcl.Diagnostics) hcl.Diagnostics {
	//var filteredDiags hcl.Diagnostics
	//for _, diag := range diags {
		//if diag.Severity == hcl.DiagError && strings.Contains(diag.Summary, "Unsupported block type") {
			//continue
		//}
		//filteredDiags = append(filteredDiags, diag)
	//}
	//return filteredDiags
//}

//// extractMarkdownSectionItems extracts items from a markdown section
//func extractMarkdownSectionItems(data, sectionName string) ([]string, error) {
	//extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	//p := parser.NewWithExtensions(extensions)
	//rootNode := markdown.Parse([]byte(data), p)

	//var items []string
	//var inTargetSection bool

	//ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		//if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			//text := strings.TrimSpace(extractText(heading))
			//if strings.EqualFold(text, sectionName) || strings.EqualFold(text, sectionName+"s") {
				//inTargetSection = true
				//return ast.GoToNext
			//}
			//inTargetSection = false
		//}

		//if inTargetSection {
			//if table, ok := node.(*ast.Table); ok && entering {
				//// Extract items from the table
				//for _, child := range table.GetChildren() {
					//if body, ok := child.(*ast.TableBody); ok {
						//for _, rowChild := range body.GetChildren() {
							//if tableRow, ok := rowChild.(*ast.TableRow); ok {
								//cells := tableRow.GetChildren()
								//if len(cells) > 0 {
									//if cell, ok := cells[0].(*ast.TableCell); ok {
										//item := extractTextFromNodes(cell.GetChildren())
										//item = strings.TrimSpace(item)
										//item = strings.Trim(item, "`") // Remove backticks if present
										//item = strings.TrimSpace(item)
										//items = append(items, item)
									//}
								//}
							//}
						//}
					//}
				//}
				//inTargetSection = false // We've processed the table, exit the section
				//return ast.SkipChildren
			//}
		//}
		//return ast.GoToNext
	//})

	//if len(items) == 0 {
		//return nil, fmt.Errorf("%s section not found or empty", sectionName)
	//}

	//return items, nil
//}

//// extractReadmeResources extracts resources and data sources from the markdown
//func extractReadmeResources(data string) ([]string, []string, error) {
	//extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	//p := parser.NewWithExtensions(extensions)
	//rootNode := markdown.Parse([]byte(data), p)

	//var resources []string
	//var dataSources []string
	//var inResourcesSection bool

	//ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		//if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			//text := strings.TrimSpace(extractText(heading))
			//if strings.EqualFold(text, "Resources") {
				//inResourcesSection = true
				//return ast.GoToNext
			//}
			//inResourcesSection = false
		//}

		//if inResourcesSection {
			//if table, ok := node.(*ast.Table); ok && entering {
				//// Extract items from the table
				//for _, child := range table.GetChildren() {
					//if body, ok := child.(*ast.TableBody); ok {
						//for _, rowChild := range body.GetChildren() {
							//if tableRow, ok := rowChild.(*ast.TableRow); ok {
								//cells := tableRow.GetChildren()
								//if len(cells) >= 2 {
									//nameCell, ok1 := cells[0].(*ast.TableCell)
									//typeCell, ok2 := cells[1].(*ast.TableCell)
									//if ok1 && ok2 {
										//name := extractTextFromNodes(nameCell.GetChildren())
										//name = strings.TrimSpace(name)
										//name = strings.Trim(name, "[]") // Remove brackets
										//name = strings.TrimSpace(name)
										//resourceType := extractTextFromNodes(typeCell.GetChildren())
										//resourceType = strings.TrimSpace(resourceType)
										//if strings.EqualFold(resourceType, "resource") {
											//resources = append(resources, name)
										//} else if strings.EqualFold(resourceType, "data source") {
											//dataSources = append(dataSources, name)
										//}
									//}
								//}
							//}
						//}
					//}
				//}
				//inResourcesSection = false // We've processed the table, exit the section
				//return ast.SkipChildren
			//}
		//}
		//return ast.GoToNext
	//})

	//if len(resources) == 0 && len(dataSources) == 0 {
		//return nil, nil, errors.New("resources section not found or empty")
	//}

	//return resources, dataSources, nil
//}

//// extractText extracts text from a node, including code spans
//func extractText(node ast.Node) string {
	//var sb strings.Builder
	//ast.WalkFunc(node, func(n ast.Node, entering bool) ast.WalkStatus {
		//if entering {
			//switch tn := n.(type) {
			//case *ast.Text:
				//sb.Write(tn.Literal)
			//case *ast.Code:
				//sb.Write(tn.Leaf.Literal)
			//}
		//}
		//return ast.GoToNext
	//})
	//return sb.String()
//}

//// extractTextFromNodes extracts text from a slice of nodes
//func extractTextFromNodes(nodes []ast.Node) string {
	//var sb strings.Builder
	//for _, node := range nodes {
		//sb.WriteString(extractText(node))
	//}
	//return sb.String()
//}

//// extractTerraformResources extracts resources and data sources from Terraform files
//func extractTerraformResources() ([]string, []string, error) {
	//var resources []string
	//var dataSources []string

	//workspace := os.Getenv("GITHUB_WORKSPACE")
	//if workspace == "" {
		//var err error
		//workspace, err = os.Getwd()
		//if err != nil {
			//return nil, nil, fmt.Errorf("failed to get current working directory: %v", err)
		//}
	//}
	//mainPath := filepath.Join(workspace, "caller", "main.tf")
	//specificResources, specificDataSources, err := extractFromFilePath(mainPath)
	//if err != nil && !os.IsNotExist(err) {
		//return nil, nil, err
	//}
	//resources = append(resources, specificResources...)
	//dataSources = append(dataSources, specificDataSources...)

	//modulesPath := filepath.Join(workspace, "caller", "modules")
	//modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
	//if err != nil {
		//return nil, nil, err
	//}
	//resources = append(resources, modulesResources...)
	//dataSources = append(dataSources, modulesDataSources...)

	//return resources, dataSources, nil
//}

//// extractRecursively extracts resources and data sources recursively
//func extractRecursively(dirPath string) ([]string, []string, error) {
	//var resources []string
	//var dataSources []string
	//if _, err := os.Stat(dirPath); os.IsNotExist(err) {
		//return resources, dataSources, nil
	//} else if err != nil {
		//return nil, nil, err
	//}
	//err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
		//if err != nil {
			//return err
		//}
		//if info.Mode().IsRegular() && filepath.Ext(path) == ".tf" {
			//fileResources, fileDataSources, err := extractFromFilePath(path)
			//if err != nil {
				//return err
			//}
			//resources = append(resources, fileResources...)
			//dataSources = append(dataSources, fileDataSources...)
		//}
		//return nil
	//})
	//if err != nil {
		//return nil, nil, err
	//}
	//return resources, dataSources, nil
//}

//// extractFromFilePath extracts resources and data sources from a Terraform file
//func extractFromFilePath(filePath string) ([]string, []string, error) {
	//content, err := os.ReadFile(filePath)
	//if err != nil {
		//return nil, nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
	//}

	//parser := hclparse.NewParser()
	//file, parseDiags := parser.ParseHCL(content, filePath)
	//if parseDiags.HasErrors() {
		//return nil, nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
	//}

	//var resources []string
	//var dataSources []string
	//body := file.Body

	//// Initialize diagnostics variable
	//var diags hcl.Diagnostics

	//// Use PartialContent to allow unknown blocks
	//hclContent, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		//Blocks: []hcl.BlockHeaderSchema{
			//{Type: "resource", LabelNames: []string{"type", "name"}},
			//{Type: "data", LabelNames: []string{"type", "name"}},
		//},
	//})

	//// Append diagnostics
	//diags = append(diags, contentDiags...)

	//// Filter out diagnostics related to unsupported block types
	//diags = filterUnsupportedBlockDiagnostics(diags)
	//if diags.HasErrors() {
		//return nil, nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	//}

	//if hclContent == nil {
		//// No relevant blocks found
		//return resources, dataSources, nil
	//}

	//for _, block := range hclContent.Blocks {
		//if len(block.Labels) >= 2 {
			//resourceType := strings.TrimSpace(block.Labels[0])
			//if block.Type == "resource" {
				//resources = append(resources, resourceType)
			//} else if block.Type == "data" {
				//dataSources = append(dataSources, resourceType)
			//}
		//}
	//}

	//return resources, dataSources, nil
//}

//// TestMarkdown runs the markdown validation tests
//func TestMarkdown(t *testing.T) {
	//readmePath := "README.md"
	//if envPath := os.Getenv("README_PATH"); envPath != "" {
		//readmePath = envPath
	//}

	//validator, err := NewMarkdownValidator(readmePath)
	//if err != nil {
		//t.Fatalf("Failed to create validator: %v", err)
	//}

	//errors := validator.Validate()
	//if len(errors) > 0 {
		//for _, err := range errors {
			//t.Errorf("Validation error: %v", err)
		//}
	//}
//}
