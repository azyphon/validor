package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
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

// Validator defines the interface for all validators
type Validator interface {
	Validate() []error
}

// MarkdownContent holds parsed markdown content
type MarkdownContent struct {
	data       string
	rootNode   ast.Node
	parser     *parser.Parser
	sections   map[string]bool
	stringPool *sync.Pool
}

// NewMarkdownContent creates a new markdown content parser
func NewMarkdownContent(data string) *MarkdownContent {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	return &MarkdownContent{
		data:     data,
		rootNode: rootNode,
		parser:   p,
		sections: make(map[string]bool),
		stringPool: &sync.Pool{
			New: func() any {
				return &strings.Builder{}
			},
		},
	}
}

// HasSection checks if a section exists in the markdown
func (mc *MarkdownContent) HasSection(sectionName string) bool {
	if found, exists := mc.sections[sectionName]; exists {
		return found
	}

	found := false
	ast.WalkFunc(mc.rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(mc.extractText(heading))
			if strings.EqualFold(text, sectionName) ||
				strings.EqualFold(text, sectionName+"s") ||
				(sectionName == "Inputs" && (strings.EqualFold(text, "Required Inputs") || strings.EqualFold(text, "Optional Inputs"))) {
				found = true
				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})

	mc.sections[sectionName] = found
	return found
}

// ExtractSectionItems extracts items from a specific section
func (mc *MarkdownContent) ExtractSectionItems(sectionNames ...string) []string {
	var items []string
	inTargetSection := false

	ast.WalkFunc(mc.rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := n.(*ast.Heading); ok && entering {
			headingText := strings.TrimSpace(mc.extractText(heading))
			if heading.Level == 2 {
				inTargetSection = false
				for _, sectionName := range sectionNames {
					if strings.EqualFold(headingText, sectionName) {
						inTargetSection = true
						break
					}
				}
			} else if heading.Level == 3 && inTargetSection {
				inputName := strings.Trim(headingText, " []")
				items = append(items, inputName)
			}
		}
		return ast.GoToNext
	})

	return items
}

// ExtractResourcesAndDataSources extracts resources and data sources from markdown
func (mc *MarkdownContent) ExtractResourcesAndDataSources() ([]string, []string, error) {
	var resources []string
	var dataSources []string
	inResourceSection := false

	ast.WalkFunc(mc.rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := n.(*ast.Heading); ok && entering {
			headingText := mc.extractText(heading)
			if strings.Contains(headingText, "Resources") {
				inResourceSection = true
			} else if heading.Level <= 2 {
				inResourceSection = false
			}
		}
		if inResourceSection && entering {
			if link, ok := n.(*ast.Link); ok {
				linkText := mc.extractText(link)
				destination := string(link.Destination)
				if strings.Contains(linkText, "azurerm_") {
					resourceName := strings.Split(linkText, "]")[0]
					resourceName = strings.TrimPrefix(resourceName, "[")
					baseName := strings.Split(resourceName, ".")[0]
					// Check if it's a data source by looking at the URL
					if strings.Contains(destination, "/data-sources/") {
						if !slices.Contains(dataSources, resourceName) {
							dataSources = append(dataSources, resourceName)
						}
						if !slices.Contains(dataSources, baseName) {
							dataSources = append(dataSources, baseName)
						}
					} else {
						if !slices.Contains(resources, resourceName) {
							resources = append(resources, resourceName)
						}
						if !slices.Contains(resources, baseName) {
							resources = append(resources, baseName)
						}
					}
				}
			}
		}
		return ast.GoToNext
	})

	if len(resources) == 0 && len(dataSources) == 0 {
		return nil, nil, errors.New("resources section not found or empty")
	}

	return resources, dataSources, nil
}

// extractText gets text content from a node
func (mc *MarkdownContent) extractText(node ast.Node) string {
	sb := mc.stringPool.Get().(*strings.Builder)
	sb.Reset()
	defer mc.stringPool.Put(sb)

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

// TerraformContent handles Terraform file parsing and content extraction
type TerraformContent struct {
	workspace  string
	parserPool *sync.Pool
	fileCache  sync.Map
}

// NewTerraformContent creates a new terraform content handler
func NewTerraformContent() (*TerraformContent, error) {
	workspace := os.Getenv("GITHUB_WORKSPACE")
	if workspace == "" {
		var err error
		workspace, err = os.Getwd()
		if err != nil {
			return nil, fmt.Errorf("failed to get current working directory: %w", err)
		}
	}

	return &TerraformContent{
		workspace: workspace,
		parserPool: &sync.Pool{
			New: func() any {
				return hclparse.NewParser()
			},
		},
	}, nil
}

// ExtractItems extracts items of specified block type from a file
func (tc *TerraformContent) ExtractItems(filePath, blockType string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, nil
		}
		return nil, fmt.Errorf("error reading file %s: %w", filepath.Base(filePath), err)
	}

	parser := tc.parserPool.Get().(*hclparse.Parser)
	defer tc.parserPool.Put(parser)

	file, parseDiags := parser.ParseHCL(content, filePath)
	if parseDiags.HasErrors() {
		return nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
	}

	var items []string
	body := file.Body
	hclContent, _, diags := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: blockType, LabelNames: []string{"name"}},
		},
	})

	if diags.HasErrors() {
		return nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	}

	if hclContent == nil {
		return items, nil
	}

	for _, block := range hclContent.Blocks {
		if len(block.Labels) > 0 {
			itemName := strings.TrimSpace(block.Labels[0])
			items = append(items, itemName)
		}
	}

	return items, nil
}

// ExtractResourcesAndDataSources extracts resources and data sources from terraform files
func (tc *TerraformContent) ExtractResourcesAndDataSources() ([]string, []string, error) {
	var (
		resources      = make([]string, 0, 32)
		dataSources    = make([]string, 0, 32)
		resourceChan   = make(chan []string, 2)
		dataSourceChan = make(chan []string, 2)
		errChan        = make(chan error, 2)
		wg             sync.WaitGroup
	)

	wg.Add(2)

	// Process main.tf concurrently
	go func() {
		defer wg.Done()
		mainPath := filepath.Join(tc.workspace, "caller", "main.tf")
		specificResources, specificDataSources, err := tc.extractFromFilePath(mainPath)
		if err != nil && !os.IsNotExist(err) {
			errChan <- err
			return
		}
		resourceChan <- specificResources
		dataSourceChan <- specificDataSources
	}()

	// Process modules concurrently
	go func() {
		defer wg.Done()
		modulesPath := filepath.Join(tc.workspace, "caller", "modules")
		modulesResources, modulesDataSources, err := tc.extractRecursively(modulesPath)
		if err != nil {
			errChan <- err
			return
		}
		resourceChan <- modulesResources
		dataSourceChan <- modulesDataSources
	}()

	go func() {
		wg.Wait()
		close(resourceChan)
		close(dataSourceChan)
		close(errChan)
	}()

	// Collect results
	for r := range resourceChan {
		resources = append(resources, r...)
	}
	for ds := range dataSourceChan {
		dataSources = append(dataSources, ds...)
	}

	// Check for errors
	for err := range errChan {
		if err != nil {
			return nil, nil, err
		}
	}

	return resources, dataSources, nil
}

// extractFromFilePath extracts resources and data sources from a specific file
func (tc *TerraformContent) extractFromFilePath(filePath string) ([]string, []string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return []string{}, []string{}, nil
		}
		return nil, nil, fmt.Errorf("error reading file %s: %w", filepath.Base(filePath), err)
	}

	parser := tc.parserPool.Get().(*hclparse.Parser)
	defer tc.parserPool.Put(parser)

	file, parseDiags := parser.ParseHCL(content, filePath)
	if parseDiags.HasErrors() {
		return nil, nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
	}

	var resources []string
	var dataSources []string
	body := file.Body
	hclContent, _, diags := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "resource", LabelNames: []string{"type", "name"}},
			{Type: "data", LabelNames: []string{"type", "name"}},
		},
	})

	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	}

	if hclContent == nil {
		return resources, dataSources, nil
	}

	for _, block := range hclContent.Blocks {
		if len(block.Labels) >= 2 {
			resourceType := strings.TrimSpace(block.Labels[0])
			resourceName := strings.TrimSpace(block.Labels[1])
			fullResourceName := resourceType + "." + resourceName

			switch block.Type {
			case "resource":
				resources = append(resources, resourceType)
				resources = append(resources, fullResourceName)
			case "data":
				dataSources = append(dataSources, resourceType)
				dataSources = append(dataSources, fullResourceName)
			}
		}
	}

	return resources, dataSources, nil
}

// extractRecursively extracts resources and data sources from all files in a directory
func (tc *TerraformContent) extractRecursively(dirPath string) ([]string, []string, error) {
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
			fileResources, fileDataSources, err := tc.extractFromFilePath(path)
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

// SectionValidator validates required sections in the markdown
type SectionValidator struct {
	content  *MarkdownContent
	sections []string
}

// NewSectionValidator creates a new section validator
func NewSectionValidator(content *MarkdownContent) *SectionValidator {
	sections := []string{
		"Goals", "Resources", "Providers", "Requirements",
		"Optional Inputs", "Required Inputs", "Outputs", "Testing",
	}
	return &SectionValidator{content: content, sections: sections}
}

// Validate checks if all required sections exist
func (sv *SectionValidator) Validate() []error {
	var allErrors []error

	// Check each required section
	for _, section := range sv.sections {
		if !sv.content.HasSection(section) {
			allErrors = append(allErrors, fmt.Errorf("required section missing: '%s'", section))
		}
	}

	return allErrors
}

// FileValidator checks for required files
type FileValidator struct {
	rootDir string
	files   []string
}

// NewFileValidator creates a new file validator
func NewFileValidator(readmePath string) *FileValidator {
	rootDir := filepath.Dir(readmePath)
	files := []string{
		readmePath,
		filepath.Join(rootDir, "outputs.tf"),
		filepath.Join(rootDir, "variables.tf"),
		filepath.Join(rootDir, "terraform.tf"),
		filepath.Join(rootDir, "Makefile"),
	}
	return &FileValidator{rootDir: rootDir, files: files}
}

// Validate checks if all required files exist and are not empty
func (fv *FileValidator) Validate() []error {
	var allErrors []error
	for _, filePath := range fv.files {
		if err := validateFile(filePath); err != nil {
			allErrors = append(allErrors, err)
		}
	}
	return allErrors
}

// URLValidator checks if all URLs in the markdown are valid
type URLValidator struct {
	content *MarkdownContent
}

// NewURLValidator creates a new URL validator
func NewURLValidator(content *MarkdownContent) *URLValidator {
	return &URLValidator{content: content}
}

// Validate checks if all URLs in the markdown are valid
func (uv *URLValidator) Validate() []error {
	rxStrict := xurls.Strict()
	urls := rxStrict.FindAllString(uv.content.data, -1)

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

// TerraformDefinitionValidator checks if all terraform resources are documented
type TerraformDefinitionValidator struct {
	markdown  *MarkdownContent
	terraform *TerraformContent
}

// NewTerraformDefinitionValidator creates a new terraform definition validator
func NewTerraformDefinitionValidator(markdown *MarkdownContent, terraform *TerraformContent) *TerraformDefinitionValidator {
	return &TerraformDefinitionValidator{
		markdown:  markdown,
		terraform: terraform,
	}
}

// Validate checks if all terraform resources are documented
func (tdv *TerraformDefinitionValidator) Validate() []error {
	tfResources, tfDataSources, err := tdv.terraform.ExtractResourcesAndDataSources()
	if err != nil {
		return []error{err}
	}

	readmeResources, readmeDataSources, err := tdv.markdown.ExtractResourcesAndDataSources()
	if err != nil {
		return []error{err}
	}

	var errors []error
	errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
	errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)

	return errors
}

// ItemValidator checks if all terraform inputs/outputs are documented
type ItemValidator struct {
	markdown  *MarkdownContent
	terraform *TerraformContent
	itemType  string
	blockType string
	sections  []string
	fileName  string
}

// NewItemValidator creates a new item validator
func NewItemValidator(markdown *MarkdownContent, terraform *TerraformContent, itemType, blockType string, sections []string, fileName string) *ItemValidator {
	return &ItemValidator{
		markdown:  markdown,
		terraform: terraform,
		itemType:  itemType,
		blockType: blockType,
		sections:  sections,
		fileName:  fileName,
	}
}

// Validate checks if all terraform inputs/outputs are documented
func (iv *ItemValidator) Validate() []error {
	// Check if any of the sections exist
	sectionExists := slices.ContainsFunc(iv.sections, func(section string) bool {
		return iv.markdown.HasSection(section)
	})

	if !sectionExists {
		return nil
	}

	filePath := filepath.Join(iv.terraform.workspace, "caller", iv.fileName)
	tfItems, err := iv.terraform.ExtractItems(filePath, iv.blockType)
	if err != nil {
		return []error{err}
	}

	var mdItems []string
	for _, section := range iv.sections {
		mdItems = append(mdItems, iv.markdown.ExtractSectionItems(section)...)
	}

	return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
}

// ReadmeValidator is the main validator that orchestrates all other validators
type ReadmeValidator struct {
	readmePath string
	markdown   *MarkdownContent
	terraform  *TerraformContent
	validators []Validator
}

// NewReadmeValidator creates a main validator that orchestrates all other validators
func NewReadmeValidator(readmePath string) (*ReadmeValidator, error) {
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	absReadmePath, err := filepath.Abs(readmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %w", err)
	}

	data, err := os.ReadFile(absReadmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %w", err)
	}

	markdown := NewMarkdownContent(string(data))

	terraform, err := NewTerraformContent()
	if err != nil {
		return nil, fmt.Errorf("failed to initialize terraform content: %w", err)
	}

	validator := &ReadmeValidator{
		readmePath: absReadmePath,
		markdown:   markdown,
		terraform:  terraform,
	}

	sectionValidator := NewSectionValidator(markdown)
	validator.validators = []Validator{
		sectionValidator,
		NewFileValidator(absReadmePath),
		NewURLValidator(markdown),
		NewTerraformDefinitionValidator(markdown, terraform),
		NewItemValidator(markdown, terraform, "Variables", "variable", []string{"Required Inputs", "Optional Inputs"}, "variables.tf"),
		NewItemValidator(markdown, terraform, "Outputs", "output", []string{"Outputs"}, "outputs.tf"),
	}

	return validator, nil
}

// Validate runs all validators and collects all errors
func (rv *ReadmeValidator) Validate() []error {
	var allErrors []error
	for _, validator := range rv.validators {
		allErrors = append(allErrors, validator.Validate()...)
	}
	return allErrors
}

// Helper functions

// validateFile checks if a file exists and is not empty
func validateFile(filePath string) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", filepath.Base(filePath))
		}
		return fmt.Errorf("error accessing file: %s: %w", filepath.Base(filePath), err)
	}
	if fileInfo.Size() == 0 {
		return fmt.Errorf("file is empty: %s", filepath.Base(filePath))
	}
	return nil
}

// validateSingleURL checks if a URL is valid
func validateSingleURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return fmt.Errorf("error accessing URL: %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("URL returned non-OK status: %s: Status: %d", url, resp.StatusCode)
	}
	return nil
}

// compareTerraformAndMarkdown compares terraform and markdown items
func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
	errors := make([]error, 0, len(tfItems)+len(mdItems))
	tfSet := make(map[string]bool, len(tfItems)*2)
	mdSet := make(map[string]bool, len(mdItems)*2)
	reported := make(map[string]bool, len(tfItems)+len(mdItems))

	getFullName := func(items []string, baseName string) string {
		for _, item := range items {
			if strings.HasPrefix(item, baseName+".") {
				return item
			}
		}
		return baseName
	}

	for _, item := range tfItems {
		tfSet[item] = true
		baseName := strings.Split(item, ".")[0]
		tfSet[baseName] = true
	}

	for _, item := range mdItems {
		mdSet[item] = true
		baseName := strings.Split(item, ".")[0]
		mdSet[baseName] = true
	}

	for _, tfItem := range tfItems {
		baseName := strings.Split(tfItem, ".")[0]
		if !mdSet[tfItem] && !mdSet[baseName] && !reported[baseName] {
			fullName := getFullName(tfItems, baseName)
			errors = append(errors, fmt.Errorf("%s in Terraform but missing in markdown: %s", itemType, fullName))
			reported[baseName] = true
		}
	}

	for _, mdItem := range mdItems {
		baseName := strings.Split(mdItem, ".")[0]
		if !tfSet[mdItem] && !tfSet[baseName] && !reported[baseName] {
			fullName := getFullName(mdItems, baseName)
			errors = append(errors, fmt.Errorf("%s in markdown but missing in Terraform: %s", itemType, fullName))
			reported[baseName] = true
		}
	}

	return errors
}

// The test function
func TestMarkdown(t *testing.T) {
	readmePath := "README.md"
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	validator, err := NewReadmeValidator(readmePath)
	if err != nil {
		t.Fatalf("Failed to create validator: %v", err)
	}

	errors := validator.Validate()
	if len(errors) > 0 {
		for _, err := range errors {
			t.Errorf("Validation error: %v", err)
		}
	}
}

// package main
//
// import (
// 	"errors"
// 	"fmt"
// 	"net/http"
// 	"os"
// 	"path/filepath"
// 	"strings"
// 	"sync"
// 	"testing"
//
// 	"github.com/gomarkdown/markdown"
// 	"github.com/gomarkdown/markdown/ast"
// 	"github.com/gomarkdown/markdown/parser"
// 	"github.com/hashicorp/hcl/v2"
// 	"github.com/hashicorp/hcl/v2/hclparse"
// 	"mvdan.cc/xurls/v2"
// )
//
// // Validator is an interface for all validators
// type Validator interface {
// 	Validate() []error
// }
//
// // MarkdownValidator orchestrates all validations
// type MarkdownValidator struct {
// 	readmePath    string
// 	data          string
// 	validators    []Validator
// 	foundSections map[string]bool
// 	hclCache      sync.Map // Cache for parsed HCL files
// }
//
// type SectionValidator struct {
// 	data     string
// 	sections []string
// 	rootNode ast.Node
// }
//
// func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
// 	if envPath := os.Getenv("README_PATH"); envPath != "" {
// 		readmePath = envPath
// 	}
// 	absReadmePath, err := filepath.Abs(readmePath)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to get absolute path: %v", err)
// 	}
// 	data, err := os.ReadFile(absReadmePath)
// 	if err != nil {
// 		return nil, fmt.Errorf("failed to read file: %v", err)
// 	}
// 	mv := &MarkdownValidator{
// 		readmePath:    absReadmePath,
// 		data:          string(data),
// 		foundSections: make(map[string]bool),
// 	}
//
// 	sectionValidator := NewSectionValidator(mv.data)
// 	mv.validators = []Validator{
// 		sectionValidator,
// 		NewFileValidator(absReadmePath),
// 		NewURLValidator(mv.data),
// 		NewTerraformDefinitionValidator(mv.data),
// 		NewItemValidator(mv.data, "Variables", "variable", []string{"Required Inputs", "Optional Inputs"}, "variables.tf"),
// 		NewItemValidator(mv.data, "Outputs", "output", []string{"Outputs"}, "outputs.tf"),
// 	}
//
// 	for _, section := range sectionValidator.sections {
// 		mv.foundSections[section] = sectionValidator.validateSection(section)
// 	}
//
// 	return mv, nil
// }
//
// func (mv *MarkdownValidator) Validate() []error {
// 	var allErrors []error
// 	for _, validator := range mv.validators {
// 		if itemValidator, ok := validator.(*ItemValidator); ok {
// 			// Check if any of the sections exist
// 			sectionExists := false
// 			for _, section := range itemValidator.sections {
// 				if mv.foundSections[section] {
// 					sectionExists = true
// 					break
// 				}
// 			}
// 			if !sectionExists {
// 				continue
// 			}
// 		}
// 		allErrors = append(allErrors, validator.Validate()...)
// 	}
// 	return allErrors
// }
//
// func (sv *SectionValidator) Validate() []error {
// 	var allErrors []error
//
// 	// Check each required section
// 	for _, section := range sv.sections {
// 		if !sv.validateSection(section) {
// 			// Add explicit error for missing section
// 			allErrors = append(allErrors, fmt.Errorf("required section missing: '%s'", section))
// 		}
// 	}
//
// 	return allErrors
// }
//
// func NewSectionValidator(data string) *SectionValidator {
// 	sections := []string{
// 		"Goals", "Resources", "Providers", "Requirements",
// 		"Optional Inputs", "Required Inputs", "Outputs", "Testing",
// 	}
//
// 	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
// 	p := parser.NewWithExtensions(extensions)
// 	rootNode := markdown.Parse([]byte(data), p)
//
// 	return &SectionValidator{data: data, sections: sections, rootNode: rootNode}
// }
//
// func (sv *SectionValidator) validateSection(sectionName string) bool {
// 	found := false
// 	ast.WalkFunc(sv.rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
// 		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
// 			text := strings.TrimSpace(extractText(heading))
// 			if strings.EqualFold(text, sectionName) ||
// 				strings.EqualFold(text, sectionName+"s") ||
// 				(sectionName == "Inputs" && (strings.EqualFold(text, "Required Inputs") || strings.EqualFold(text, "Optional Inputs"))) {
// 				found = true
// 				return ast.SkipChildren
// 			}
// 		}
// 		return ast.GoToNext
// 	})
// 	return found
// }
//
// // FileValidator validates the presence of required files
// type FileValidator struct {
// 	files []string
// }
//
// func NewFileValidator(readmePath string) *FileValidator {
// 	rootDir := filepath.Dir(readmePath)
// 	files := []string{
// 		readmePath,
// 		filepath.Join(rootDir, "outputs.tf"),
// 		filepath.Join(rootDir, "variables.tf"),
// 		filepath.Join(rootDir, "terraform.tf"),
// 		filepath.Join(rootDir, "Makefile"),
// 	}
// 	return &FileValidator{files: files}
// }
//
// func (fv *FileValidator) Validate() []error {
// 	var allErrors []error
// 	for _, filePath := range fv.files {
// 		if err := validateFile(filePath); err != nil {
// 			allErrors = append(allErrors, err)
// 		}
// 	}
// 	return allErrors
// }
//
// func validateFile(filePath string) error {
// 	fileInfo, err := os.Stat(filePath)
// 	if err != nil {
// 		if os.IsNotExist(err) {
// 			return fmt.Errorf("file does not exist: %s", filepath.Base(filePath))
// 		}
// 		return fmt.Errorf("error accessing file: %s: %v", filepath.Base(filePath), err)
// 	}
//
// 	if fileInfo.Size() == 0 {
// 		return fmt.Errorf("file is empty: %s", filepath.Base(filePath))
// 	}
//
// 	return nil
// }
//
// type URLValidator struct {
// 	data string
// }
//
// func NewURLValidator(data string) *URLValidator {
// 	return &URLValidator{data: data}
// }
//
// func (uv *URLValidator) Validate() []error {
// 	rxStrict := xurls.Strict()
// 	urls := rxStrict.FindAllString(uv.data, -1)
//
// 	var wg sync.WaitGroup
// 	errChan := make(chan error, len(urls))
//
// 	for _, u := range urls {
// 		if strings.Contains(u, "registry.terraform.io/providers/") {
// 			continue
// 		}
//
// 		wg.Add(1)
// 		go func(url string) {
// 			defer wg.Done()
// 			if err := validateSingleURL(url); err != nil {
// 				errChan <- err
// 			}
// 		}(u)
// 	}
//
// 	wg.Wait()
// 	close(errChan)
//
// 	var errors []error
// 	for err := range errChan {
// 		errors = append(errors, err)
// 	}
//
// 	return errors
// }
//
// func validateSingleURL(url string) error {
// 	resp, err := http.Get(url)
// 	if err != nil {
// 		return fmt.Errorf("error accessing URL: %s: %v", url, err)
// 	}
// 	defer resp.Body.Close()
//
// 	if resp.StatusCode != http.StatusOK {
// 		return fmt.Errorf("URL returned non-OK status: %s: Status: %d", url, resp.StatusCode)
// 	}
//
// 	return nil
// }
//
// // TerraformDefinitionValidator validates Terraform definitions
// type TerraformDefinitionValidator struct {
// 	data string
// }
//
// func NewTerraformDefinitionValidator(data string) *TerraformDefinitionValidator {
// 	return &TerraformDefinitionValidator{data: data}
// }
//
// func (tdv *TerraformDefinitionValidator) Validate() []error {
// 	tfResources, tfDataSources, err := extractTerraformResources()
// 	if err != nil {
// 		return []error{err}
// 	}
//
// 	readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
// 	if err != nil {
// 		return []error{err}
// 	}
//
// 	var errors []error
// 	errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
// 	errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)
//
// 	return errors
// }
//
// type ItemValidator struct {
// 	data      string
// 	itemType  string
// 	blockType string
// 	sections  []string
// 	fileName  string
// }
//
// func NewItemValidator(data, itemType, blockType string, sections []string, fileName string) *ItemValidator {
// 	return &ItemValidator{
// 		data:      data,
// 		itemType:  itemType,
// 		blockType: blockType,
// 		sections:  sections,
// 		fileName:  fileName,
// 	}
// }
//
// func (iv *ItemValidator) Validate() []error {
// 	workspace := os.Getenv("GITHUB_WORKSPACE")
// 	if workspace == "" {
// 		var err error
// 		workspace, err = os.Getwd()
// 		if err != nil {
// 			return []error{fmt.Errorf("failed to get current working directory: %v", err)}
// 		}
// 	}
// 	filePath := filepath.Join(workspace, "caller", iv.fileName)
// 	tfItems, err := extractTerraformItems(filePath, iv.blockType)
// 	if err != nil {
// 		return []error{err}
// 	}
//
// 	var mdItems []string
// 	for _, section := range iv.sections {
// 		mdItems = append(mdItems, extractMarkdownSectionItems(iv.data, section)...)
// 	}
//
// 	return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
// }
//
// var stringBuilderPool = sync.Pool{
// 	New: func() interface{} {
// 		return &strings.Builder{}
// 	},
// }
//
// func extractText(node ast.Node) string {
// 	sb := stringBuilderPool.Get().(*strings.Builder)
// 	sb.Reset()
// 	defer stringBuilderPool.Put(sb)
//
// 	ast.WalkFunc(node, func(n ast.Node, entering bool) ast.WalkStatus {
// 		if entering {
// 			switch tn := n.(type) {
// 			case *ast.Text:
// 				sb.Write(tn.Literal)
// 			case *ast.Code:
// 				sb.Write(tn.Literal)
// 			}
// 		}
// 		return ast.GoToNext
// 	})
// 	return sb.String()
// }
//
// func extractTerraformItems(filePath string, blockType string) ([]string, error) {
// 	content, err := os.ReadFile(filePath)
// 	if err != nil {
// 		return nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
// 	}
//
// 	parser := hclparse.NewParser()
// 	file, parseDiags := parser.ParseHCL(content, filePath)
// 	if parseDiags.HasErrors() {
// 		return nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
// 	}
//
// 	var items []string
// 	body := file.Body
//
// 	hclContent, _, _ := body.PartialContent(&hcl.BodySchema{
// 		Blocks: []hcl.BlockHeaderSchema{
// 			{Type: blockType, LabelNames: []string{"name"}},
// 		},
// 	})
//
// 	if hclContent == nil {
// 		return items, nil
// 	}
//
// 	for _, block := range hclContent.Blocks {
// 		if len(block.Labels) > 0 {
// 			itemName := strings.TrimSpace(block.Labels[0])
// 			items = append(items, itemName)
// 		}
// 	}
//
// 	return items, nil
// }
//
// func extractTerraformResources() ([]string, []string, error) {
// 	var (
// 		resources      = make([]string, 0, 32)
// 		dataSources    = make([]string, 0, 32)
// 		resourceChan   = make(chan []string, 2)
// 		dataSourceChan = make(chan []string, 2)
// 		errChan        = make(chan error, 2)
// 		wg             sync.WaitGroup
// 	)
//
// 	workspace := os.Getenv("GITHUB_WORKSPACE")
// 	if workspace == "" {
// 		var err error
// 		workspace, err = os.Getwd()
// 		if err != nil {
// 			return nil, nil, fmt.Errorf("failed to get current working directory: %v", err)
// 		}
// 	}
//
// 	wg.Add(2)
// 	// Process main.tf concurrently
// 	go func() {
// 		defer wg.Done()
// 		mainPath := filepath.Join(workspace, "caller", "main.tf")
// 		specificResources, specificDataSources, err := extractFromFilePath(mainPath)
// 		if err != nil && !os.IsNotExist(err) {
// 			errChan <- err
// 			return
// 		}
// 		resourceChan <- specificResources
// 		dataSourceChan <- specificDataSources
// 	}()
//
// 	// Process modules concurrently
// 	go func() {
// 		defer wg.Done()
// 		modulesPath := filepath.Join(workspace, "caller", "modules")
// 		modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
// 		if err != nil {
// 			errChan <- err
// 			return
// 		}
// 		resourceChan <- modulesResources
// 		dataSourceChan <- modulesDataSources
// 	}()
//
// 	go func() {
// 		wg.Wait()
// 		close(resourceChan)
// 		close(dataSourceChan)
// 		close(errChan)
// 	}()
//
// 	// Collect results
// 	for r := range resourceChan {
// 		resources = append(resources, r...)
// 	}
// 	for ds := range dataSourceChan {
// 		dataSources = append(dataSources, ds...)
// 	}
//
// 	// Check for errors
// 	if err := <-errChan; err != nil {
// 		return nil, nil, err
// 	}
//
// 	return resources, dataSources, nil
// }
//
// func extractRecursively(dirPath string) ([]string, []string, error) {
// 	var resources []string
// 	var dataSources []string
// 	if _, err := os.Stat(dirPath); os.IsNotExist(err) {
// 		return resources, dataSources, nil
// 	} else if err != nil {
// 		return nil, nil, err
// 	}
// 	err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
// 		if err != nil {
// 			return err
// 		}
// 		if info.Mode().IsRegular() && filepath.Ext(path) == ".tf" {
// 			fileResources, fileDataSources, err := extractFromFilePath(path)
// 			if err != nil {
// 				return err
// 			}
// 			resources = append(resources, fileResources...)
// 			dataSources = append(dataSources, fileDataSources...)
// 		}
// 		return nil
// 	})
// 	if err != nil {
// 		return nil, nil, err
// 	}
// 	return resources, dataSources, nil
// }
//
// var hclParserPool = sync.Pool{
// 	New: func() interface{} {
// 		return hclparse.NewParser()
// 	},
// }
//
// func extractFromFilePath(filePath string) ([]string, []string, error) {
// 	content, err := os.ReadFile(filePath)
// 	if err != nil {
// 		return nil, nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
// 	}
//
// 	parser := hclParserPool.Get().(*hclparse.Parser)
// 	defer hclParserPool.Put(parser)
//
// 	file, parseDiags := parser.ParseHCL(content, filePath)
// 	if parseDiags.HasErrors() {
// 		return nil, nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), parseDiags)
// 	}
//
// 	var resources []string
// 	var dataSources []string
// 	body := file.Body
//
// 	hclContent, _, diags := body.PartialContent(&hcl.BodySchema{
// 		Blocks: []hcl.BlockHeaderSchema{
// 			{Type: "resource", LabelNames: []string{"type", "name"}},
// 			{Type: "data", LabelNames: []string{"type", "name"}},
// 		},
// 	})
//
// 	if diags.HasErrors() {
// 		return nil, nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
// 	}
//
// 	if hclContent == nil {
// 		return resources, dataSources, nil
// 	}
//
// 	for _, block := range hclContent.Blocks {
// 		if len(block.Labels) >= 2 {
// 			resourceType := strings.TrimSpace(block.Labels[0])
// 			resourceName := strings.TrimSpace(block.Labels[1])
// 			fullResourceName := resourceType + "." + resourceName
// 			switch block.Type {
// 			case "resource":
// 				resources = append(resources, resourceType)
// 				resources = append(resources, fullResourceName)
// 			case "data":
// 				dataSources = append(dataSources, resourceType)
// 				dataSources = append(dataSources, fullResourceName)
// 			}
// 		}
// 	}
//
// 	return resources, dataSources, nil
// }
//
// func extractMarkdownSectionItems(data string, sectionNames ...string) []string {
// 	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
// 	p := parser.NewWithExtensions(extensions)
// 	rootNode := p.Parse([]byte(data))
//
// 	var items []string
// 	inTargetSection := false
//
// 	ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
// 		if heading, ok := n.(*ast.Heading); ok && entering {
// 			headingText := strings.TrimSpace(extractText(heading))
// 			if heading.Level == 2 {
// 				inTargetSection = false
// 				for _, sectionName := range sectionNames {
// 					if strings.EqualFold(headingText, sectionName) {
// 						inTargetSection = true
// 						break
// 					}
// 				}
// 			} else if heading.Level == 3 && inTargetSection {
// 				inputName := strings.Trim(headingText, " []")
// 				items = append(items, inputName)
// 			}
// 		}
// 		return ast.GoToNext
// 	})
//
// 	return items
// }
//
// func extractReadmeResources(data string) ([]string, []string, error) {
// 	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
// 	p := parser.NewWithExtensions(extensions)
// 	rootNode := p.Parse([]byte(data))
//
// 	var resources []string
// 	var dataSources []string
// 	inResourceSection := false
//
// 	ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
// 		if heading, ok := n.(*ast.Heading); ok && entering {
// 			headingText := extractText(heading)
// 			if strings.Contains(headingText, "Resources") {
// 				inResourceSection = true
// 			} else if heading.Level <= 2 {
// 				inResourceSection = false
// 			}
// 		}
//
// 		if inResourceSection && entering {
// 			if link, ok := n.(*ast.Link); ok {
// 				linkText := extractText(link)
// 				destination := string(link.Destination)
//
// 				if strings.Contains(linkText, "azurerm_") {
// 					resourceName := strings.Split(linkText, "]")[0]
// 					resourceName = strings.TrimPrefix(resourceName, "[")
//
// 					baseName := strings.Split(resourceName, ".")[0]
//
// 					// Check if it's a data source by looking at the URL
// 					if strings.Contains(destination, "/data-sources/") {
// 						if !contains(dataSources, resourceName) {
// 							dataSources = append(dataSources, resourceName)
// 						}
// 						if !contains(dataSources, baseName) {
// 							dataSources = append(dataSources, baseName)
// 						}
// 					} else {
// 						if !contains(resources, resourceName) {
// 							resources = append(resources, resourceName)
// 						}
// 						if !contains(resources, baseName) {
// 							resources = append(resources, baseName)
// 						}
// 					}
// 				}
// 			}
// 		}
// 		return ast.GoToNext
// 	})
//
// 	if len(resources) == 0 && len(dataSources) == 0 {
// 		return nil, nil, errors.New("resources section not found or empty")
// 	}
//
// 	return resources, dataSources, nil
// }
//
// // Helper function to check if a string slice contains a value
// func contains(slice []string, str string) bool {
// 	for _, v := range slice {
// 		if v == str {
// 			return true
// 		}
// 	}
// 	return false
// }
//
// func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
// 	errors := make([]error, 0, len(tfItems)+len(mdItems))
// 	tfSet := make(map[string]bool, len(tfItems)*2)
// 	mdSet := make(map[string]bool, len(mdItems)*2)
// 	reported := make(map[string]bool, len(tfItems)+len(mdItems))
//
// 	getFullName := func(items []string, baseName string) string {
// 		for _, item := range items {
// 			if strings.HasPrefix(item, baseName+".") {
// 				return item
// 			}
// 		}
// 		return baseName
// 	}
//
// 	for _, item := range tfItems {
// 		tfSet[item] = true
// 		baseName := strings.Split(item, ".")[0]
// 		tfSet[baseName] = true
// 	}
// 	for _, item := range mdItems {
// 		mdSet[item] = true
// 		baseName := strings.Split(item, ".")[0]
// 		mdSet[baseName] = true
// 	}
//
// 	for _, tfItem := range tfItems {
// 		baseName := strings.Split(tfItem, ".")[0]
// 		if !mdSet[tfItem] && !mdSet[baseName] && !reported[baseName] {
// 			fullName := getFullName(tfItems, baseName)
// 			errors = append(errors, fmt.Errorf("%s in Terraform but missing in markdown: %s", itemType, fullName))
// 			reported[baseName] = true
// 		}
// 	}
//
// 	for _, mdItem := range mdItems {
// 		baseName := strings.Split(mdItem, ".")[0]
// 		if !tfSet[mdItem] && !tfSet[baseName] && !reported[baseName] {
// 			fullName := getFullName(mdItems, baseName)
// 			errors = append(errors, fmt.Errorf("%s in markdown but missing in Terraform: %s", itemType, fullName))
// 			reported[baseName] = true
// 		}
// 	}
//
// 	return errors
// }
//
// func TestMarkdown(t *testing.T) {
// 	readmePath := "README.md"
// 	if envPath := os.Getenv("README_PATH"); envPath != "" {
// 		readmePath = envPath
// 	}
//
// 	validator, err := NewMarkdownValidator(readmePath)
// 	if err != nil {
// 		t.Fatalf("Failed to create validator: %v", err)
// 	}
//
// 	errors := validator.Validate()
// 	if len(errors) > 0 {
// 		for _, err := range errors {
// 			t.Errorf("Validation error: %v", err)
// 		}
// 	}
// }
