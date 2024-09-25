package main

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
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

	mv := &MarkdownValidator{
		readmePath: absReadmePath,
		data:       data,
	}

	// Initialize validators
	mv.validators = []Validator{
		NewSectionValidator(data),
		NewFileValidator(absReadmePath),
		NewURLValidator(data),
		NewTerraformDefinitionValidator(data),
		NewItemValidator(data, "Variables", "variable", "Inputs", "variables.tf"),
		NewItemValidator(data, "Outputs", "output", "Outputs", "outputs.tf"),
		NewTerraformRequirementsValidator(data, filepath.Dir(absReadmePath)),
	}

	return mv, nil
}

// Validate runs all registered validators
func (mv *MarkdownValidator) Validate() []error {
	var allErrors []error
	for _, validator := range mv.validators {
		allErrors = append(allErrors, validator.Validate()...)
	}
	return allErrors
}

// Section represents a markdown section
type Section struct {
	Header  string
	Columns []string
}

// SectionValidator validates markdown sections
type SectionValidator struct {
	data     string
	sections []Section
	rootNode ast.Node
}

// NewSectionValidator creates a new SectionValidator
func NewSectionValidator(data string) *SectionValidator {
	sections := []Section{
		{Header: "Goals"},
		{Header: "Resources", Columns: []string{"Name", "Type"}},
		{Header: "Providers", Columns: []string{"Name", "Version"}},
		{Header: "Requirements", Columns: []string{"Name", "Version"}},
		{Header: "Inputs", Columns: []string{"Name", "Description", "Type", "Required"}},
		{Header: "Outputs", Columns: []string{"Name", "Description"}},
		{Header: "Features"},
		{Header: "Testing"},
		{Header: "Authors"},
		{Header: "License"},
	}

	// Parse the markdown content into an AST
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	return &SectionValidator{
		data:     data,
		sections: sections,
		rootNode: rootNode,
	}
}

// Validate validates the sections in the markdown
func (sv *SectionValidator) Validate() []error {
	var allErrors []error
	for _, section := range sv.sections {
		allErrors = append(allErrors, section.validate(sv.rootNode)...)
	}
	return allErrors
}

// validate checks if a section and its columns are correctly formatted
func (s Section) validate(rootNode ast.Node) []error {
	var errors []error
	found := false

	// Traverse the AST to find the section header
	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, s.Header) || strings.EqualFold(text, s.Header+"s") {
				found = true

				if len(s.Columns) > 0 {
					// Check for the table after the header
					nextNode := getNextSibling(node)
					if table, ok := nextNode.(*ast.Table); ok {
						// Extract table headers
						actualHeaders, err := extractTableHeaders(table)
						if err != nil {
							errors = append(errors, err)
						} else if !equalSlices(actualHeaders, s.Columns) {
							errors = append(errors, compareColumns(s.Header, s.Columns, actualHeaders))
						}
					} else {
						errors = append(errors, formatError("missing table after header: %s", s.Header))
					}
				}
				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})

	if !found {
		errors = append(errors, compareHeaders(s.Header, ""))
	}

	return errors
}

// getNextSibling returns the next sibling of a node
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

// extractTableHeaders extracts headers from a markdown table
func extractTableHeaders(table *ast.Table) ([]string, error) {
	headers := []string{}

	if len(table.GetChildren()) == 0 {
		return nil, fmt.Errorf("table is empty")
	}

	// The first child should be TableHeader
	if headerNode, ok := table.GetChildren()[0].(*ast.TableHeader); ok {
		for _, rowNode := range headerNode.GetChildren() {
			if row, ok := rowNode.(*ast.TableRow); ok {
				for _, cellNode := range row.GetChildren() {
					if cell, ok := cellNode.(*ast.TableCell); ok {
						headerText := strings.TrimSpace(extractTextFromNodes(cell.GetChildren()))
						headers = append(headers, headerText)
					}
				}
			}
		}
	} else {
		return nil, fmt.Errorf("table has no header row")
	}

	return headers, nil
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

// validateFile checks if a file exists and is not empty
func validateFile(filePath string) []error {
	var errors []error
	fileInfo, err := os.Stat(filePath)
	baseName := filepath.Base(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			errors = append(errors, formatError("file does not exist:\n  %s", baseName))
		} else {
			errors = append(errors, formatError("error accessing file:\n  %s\n  %v", baseName, err))
		}
		return errors
	}

	if fileInfo.Size() == 0 {
		errors = append(errors, formatError("file is empty:\n  %s", baseName))
	}

	return errors
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

// validateURLs checks if URLs in the data are accessible
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

// validateSingleURL checks if a single URL is accessible
func validateSingleURL(url string) error {
	resp, err := http.Get(url)
	if err != nil {
		return formatError("error accessing URL:\n  %s\n  %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return formatError("URL returned non-OK status:\n  %s\n  Status: %d", url, resp.StatusCode)
	}

	return nil
}

// TerraformDefinitionValidator validates Terraform definitions
type TerraformDefinitionValidator struct {
	data string
}

// NewTerraformDefinitionValidator creates a new TerraformDefinitionValidator
func NewTerraformDefinitionValidator(data string) *TerraformDefinitionValidator {
	return &TerraformDefinitionValidator{data: data}
}

// Validate compares Terraform resources with those documented in the markdown
func (tdv *TerraformDefinitionValidator) Validate() []error {
	tfResources, tfDataSources, err := extractTerraformResources()
	if err != nil {
		return []error{err}
	}

	readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
	if err != nil {
		return []error{err}
	}

	var errors []error
	errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
	errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)

	return errors
}

// ItemValidator validates items in Terraform and markdown
type ItemValidator struct {
	data      string
	itemType  string
	blockType string
	section   string
	fileName  string
}

// NewItemValidator creates a new ItemValidator
func NewItemValidator(data, itemType, blockType, section, fileName string) *ItemValidator {
	return &ItemValidator{
		data:      data,
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
	filePath := filepath.Join(workspace, iv.fileName)
	tfItems, err := extractTerraformItems(filePath, iv.blockType)
	if err != nil {
		return []error{err}
	}

	mdItems, err := extractMarkdownSectionItems(iv.data, iv.section)
	if err != nil {
		return []error{err}
	}

	return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
}

// TerraformRequirementsValidator validates Terraform requirements and providers
type TerraformRequirementsValidator struct {
	data         string
	terraformDir string
}

// NewTerraformRequirementsValidator creates a new TerraformRequirementsValidator
func NewTerraformRequirementsValidator(data, terraformDir string) *TerraformRequirementsValidator {
	return &TerraformRequirementsValidator{
		data:         data,
		terraformDir: terraformDir,
	}
}

// Validate compares Terraform requirements and providers with those documented in the markdown
func (trv *TerraformRequirementsValidator) Validate() []error {
	// Extract required_version and required_providers from terraform.tf
	tfRequirements, tfProviders, err := extractTerraformRequirements(trv.terraformDir)
	if err != nil {
		return []error{err}
	}

	// Extract Requirements and Providers from the README
	mdRequirements, err := extractReadmeRequirements(trv.data)
	if err != nil {
		return []error{err}
	}

	mdProviders, err := extractReadmeProviders(trv.data)
	if err != nil {
		return []error{err}
	}

	// Compare them
	var errors []error
	errors = append(errors, compareRequirements(tfRequirements, mdRequirements)...)
	errors = append(errors, compareProviders(tfProviders, mdProviders)...)

	return errors
}

// Helper functions

// compareHeaders compares expected and actual headers
func compareHeaders(expected, actual string) error {
	if expected != actual {
		if actual == "" {
			return formatError("incorrect header:\n  expected '%s', found 'not present'", expected)
		}
		return formatError("incorrect header:\n  expected '%s', found '%s'", expected, actual)
	}
	return nil
}

// formatError formats an error message
func formatError(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

// equalSlices checks if two slices are equal
func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i, v := range a {
		if v != b[i] {
			return false
		}
	}
	return true
}

// compareColumns compares expected and actual table columns
func compareColumns(header string, expected, actual []string) error {
	var mismatches []string
	for i := 0; i < len(expected) || i < len(actual); i++ {
		var exp, act string
		if i < len(expected) {
			exp = expected[i]
		}
		if i < len(actual) {
			act = actual[i]
		}
		if exp != act {
			mismatches = append(mismatches, fmt.Sprintf("expected '%s', found '%s'", exp, act))
		}
	}
	return formatError("table under header: %s has incorrect column names:\n  %s", header, strings.Join(mismatches, "\n  "))
}

// findMissingItems finds items in a that are not in b
func findMissingItems(a, b []string) []string {
	bSet := make(map[string]struct{}, len(b))
	for _, x := range b {
		bSet[x] = struct{}{}
	}
	var missing []string
	for _, x := range a {
		if _, found := bSet[x]; !found {
			missing = append(missing, x)
		}
	}
	return missing
}

// compareTerraformAndMarkdown compares items in Terraform and markdown
func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
	var errors []error

	missingInMarkdown := findMissingItems(tfItems, mdItems)
	if len(missingInMarkdown) > 0 {
		errors = append(errors, formatError("%s missing in markdown:\n  %s", itemType, strings.Join(missingInMarkdown, "\n  ")))
	}

	missingInTerraform := findMissingItems(mdItems, tfItems)
	if len(missingInTerraform) > 0 {
		errors = append(errors, formatError("%s in markdown but missing in Terraform:\n  %s", itemType, strings.Join(missingInTerraform, "\n  ")))
	}

	return errors
}

// extractTerraformItems extracts item names from a Terraform file given the block type
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

	// Use PartialContent to extract only the specified block type
	hclContent, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: blockType, LabelNames: []string{"name"}},
		},
	})

	// Filter out diagnostics related to unsupported block types
	diags := filterUnsupportedBlockDiagnostics(contentDiags)
	if diags.HasErrors() {
		return nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
	}

	if hclContent == nil {
		// No relevant blocks found
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

// filterUnsupportedBlockDiagnostics filters out diagnostics related to unsupported block types
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

// extractMarkdownSectionItems extracts items from a markdown section
func extractMarkdownSectionItems(data, sectionName string) ([]string, error) {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	var items []string
	var inTargetSection bool

	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, sectionName) || strings.EqualFold(text, sectionName+"s") {
				inTargetSection = true
				return ast.GoToNext
			}
			inTargetSection = false
		}

		if inTargetSection {
			if table, ok := node.(*ast.Table); ok && entering {
				// Extract items from the table
				for _, child := range table.GetChildren() {
					if body, ok := child.(*ast.TableBody); ok {
						for _, rowChild := range body.GetChildren() {
							if tableRow, ok := rowChild.(*ast.TableRow); ok {
								cells := tableRow.GetChildren()
								if len(cells) > 0 {
									if cell, ok := cells[0].(*ast.TableCell); ok {
										item := extractTextFromNodes(cell.GetChildren())
										item = strings.TrimSpace(item)
										item = strings.Trim(item, "`") // Remove backticks if present
										item = strings.TrimSpace(item)
										items = append(items, item)
									}
								}
							}
						}
					}
				}
				inTargetSection = false // We've processed the table, exit the section
				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})

	if len(items) == 0 {
		return nil, fmt.Errorf("%s section not found or empty", sectionName)
	}

	return items, nil
}

// extractReadmeResources extracts resources and data sources from the markdown
func extractReadmeResources(data string) ([]string, []string, error) {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	var resources []string
	var dataSources []string
	var inResourcesSection bool

	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, "Resources") {
				inResourcesSection = true
				return ast.GoToNext
			}
			inResourcesSection = false
		}

		if inResourcesSection {
			if table, ok := node.(*ast.Table); ok && entering {
				// Extract items from the table
				for _, child := range table.GetChildren() {
					if body, ok := child.(*ast.TableBody); ok {
						for _, rowChild := range body.GetChildren() {
							if tableRow, ok := rowChild.(*ast.TableRow); ok {
								cells := tableRow.GetChildren()
								if len(cells) >= 2 {
									nameCell, ok1 := cells[0].(*ast.TableCell)
									typeCell, ok2 := cells[1].(*ast.TableCell)
									if ok1 && ok2 {
										name := extractTextFromNodes(nameCell.GetChildren())
										name = strings.TrimSpace(name)
										name = strings.Trim(name, "[]") // Remove brackets
										name = strings.TrimSpace(name)
										resourceType := extractTextFromNodes(typeCell.GetChildren())
										resourceType = strings.TrimSpace(resourceType)
										if strings.EqualFold(resourceType, "resource") {
											resources = append(resources, name)
										} else if strings.EqualFold(resourceType, "data source") {
											dataSources = append(dataSources, name)
										}
									}
								}
							}
						}
					}
				}
				inResourcesSection = false // We've processed the table, exit the section
				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})

	if len(resources) == 0 && len(dataSources) == 0 {
		return nil, nil, errors.New("resources section not found or empty")
	}

	return resources, dataSources, nil
}

// extractText extracts text from a node, including code spans
func extractText(node ast.Node) string {
	var sb strings.Builder
	ast.WalkFunc(node, func(n ast.Node, entering bool) ast.WalkStatus {
		if entering {
			switch tn := n.(type) {
			case *ast.Text:
				sb.Write(tn.Literal)
			case *ast.Code:
				sb.Write(tn.Leaf.Literal)
			}
		}
		return ast.GoToNext
	})
	return sb.String()
}

// extractTextFromNodes extracts text from a slice of nodes
func extractTextFromNodes(nodes []ast.Node) string {
	var sb strings.Builder
	for _, node := range nodes {
		sb.WriteString(extractText(node))
	}
	return sb.String()
}

// extractTerraformResources extracts resources and data sources from Terraform files
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
	mainPath := filepath.Join(workspace, "main.tf")
	specificResources, specificDataSources, err := extractFromFilePath(mainPath)
	if err != nil && !os.IsNotExist(err) {
		return nil, nil, err
	}
	resources = append(resources, specificResources...)
	dataSources = append(dataSources, specificDataSources...)

	modulesPath := filepath.Join(workspace, "modules")
	modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
	if err != nil {
		return nil, nil, err
	}
	resources = append(resources, modulesResources...)
	dataSources = append(dataSources, modulesDataSources...)

	return resources, dataSources, nil
}

// extractRecursively extracts resources and data sources recursively
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

// extractFromFilePath extracts resources and data sources from a Terraform file
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

	// Use PartialContent to allow unknown blocks
	hclContent, _, contentDiags := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "resource", LabelNames: []string{"type", "name"}},
			{Type: "data", LabelNames: []string{"type", "name"}},
		},
	})

	// Filter out diagnostics related to unsupported block types
	diags := filterUnsupportedBlockDiagnostics(contentDiags)
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

// extractTerraformRequirements extracts required_version and required_providers from terraform.tf
func extractTerraformRequirements(terraformDir string) (map[string]string, map[string]string, error) {
	// Read terraform.tf
	terraformFilePath := filepath.Join(terraformDir, "terraform.tf")
	fileContent, err := os.ReadFile(terraformFilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("error reading file terraform.tf: %v", err)
	}

	parser := hclparse.NewParser()
	file, parseDiags := parser.ParseHCL(fileContent, terraformFilePath)
	if parseDiags.HasErrors() {
		return nil, nil, fmt.Errorf("error parsing HCL in terraform.tf: %v", parseDiags)
	}

	body := file.Body

	// Extract required_version and required_providers
	contentSchema := &hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "terraform"},
		},
	}

	bodyContent, _, diags := body.PartialContent(contentSchema)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("error parsing terraform block in terraform.tf: %v", diags)
	}

	if len(bodyContent.Blocks) == 0 {
		return nil, nil, fmt.Errorf("terraform block not found in terraform.tf")
	}

	terraformBlock := bodyContent.Blocks[0]
	terraformBody := terraformBlock.Body

	// Extract required_version and required_providers
	requiredVersion := ""
	requiredProviders := make(map[string]string)

	contentSchema = &hcl.BodySchema{
		Attributes: []hcl.AttributeSchema{
			{Name: "required_version"},
		},
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "required_providers"},
		},
	}

	bodyContent, _, diags = terraformBody.PartialContent(contentSchema)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("error parsing content of terraform block in terraform.tf: %v", diags)
	}

	if attr, ok := bodyContent.Attributes["required_version"]; ok {
		// Evaluate the expression
		expr := attr.Expr
		val, diags := expr.Value(nil)
		if diags.HasErrors() {
			return nil, nil, fmt.Errorf("error evaluating required_version: %v", diags)
		}
		requiredVersion = val.AsString()
	}

	if len(bodyContent.Blocks) > 0 {
		requiredProvidersBlock := bodyContent.Blocks[0]
		requiredProvidersBody := requiredProvidersBlock.Body

		// Get all attributes (providers)
		contentSchema := &hcl.BodySchema{
			Attributes: []hcl.AttributeSchema{},
		}
		providerContent, _, diags := requiredProvidersBody.PartialContent(contentSchema)
		if diags.HasErrors() {
			return nil, nil, fmt.Errorf("error parsing required_providers block: %v", diags)
		}

		// Since we don't know the attribute names ahead of time, we need to use providerContent.Attributes
		for name, attr := range providerContent.Attributes {
			// Evaluate the expression
			expr := attr.Expr
			val, diags := expr.Value(nil)
			if diags.HasErrors() {
				return nil, nil, fmt.Errorf("error evaluating provider %s: %v", name, diags)
			}

			if val.Type().IsObjectType() || val.Type().IsMapType() {
				// It's an object, get the "version" attribute
				versionVal := val.AsValueMap()["version"]
				if !versionVal.IsNull() {
					version := versionVal.AsString()
					requiredProviders[name] = version
				} else {
					return nil, nil, fmt.Errorf("provider %s does not have a 'version' attribute", name)
				}
			} else if val.Type().FriendlyName() == "string" {
				// It's a string, treat it as the version
				requiredProviders[name] = val.AsString()
			} else {
				return nil, nil, fmt.Errorf("unexpected type for provider %s: %s", name, val.Type().GoString())
			}
		}
	}

	// Return requiredVersion and requiredProviders
	requirements := make(map[string]string)
	if requiredVersion != "" {
		requirements["terraform"] = requiredVersion
	}

	return requirements, requiredProviders, nil
}

// extractReadmeRequirements extracts requirements from the README
func extractReadmeRequirements(data string) (map[string]string, error) {
	return extractReadmeNameVersionTable(data, "Requirements")
}

// extractReadmeProviders extracts providers from the README
func extractReadmeProviders(data string) (map[string]string, error) {
	return extractReadmeNameVersionTable(data, "Providers")
}

// extractReadmeNameVersionTable extracts a name-version map from a markdown table in a specific section
func extractReadmeNameVersionTable(data, sectionName string) (map[string]string, error) {
	// Parse the markdown and find the section
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	var inTargetSection bool
	var nameVersionMap = make(map[string]string)

	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, sectionName) {
				inTargetSection = true
				return ast.GoToNext
			}
			inTargetSection = false
		}

		if inTargetSection {
			if table, ok := node.(*ast.Table); ok && entering {
				// Extract items from the table
				// We expect columns: Name | Version
				var headers []string
				var rows [][]string

				for _, child := range table.GetChildren() {
					switch child := child.(type) {
					case *ast.TableHeader:
						for _, rowChild := range child.GetChildren() {
							if tableRow, ok := rowChild.(*ast.TableRow); ok {
								var row []string
								for _, cellNode := range tableRow.GetChildren() {
									if cell, ok := cellNode.(*ast.TableCell); ok {
										text := extractTextFromNodes(cell.GetChildren())
										text = strings.TrimSpace(text)
										row = append(row, text)
									}
								}
								headers = row
							}
						}
					case *ast.TableBody:
						for _, rowChild := range child.GetChildren() {
							if tableRow, ok := rowChild.(*ast.TableRow); ok {
								var row []string
								for _, cellNode := range tableRow.GetChildren() {
									if cell, ok := cellNode.(*ast.TableCell); ok {
										text := extractTextFromNodes(cell.GetChildren())
										text = strings.TrimSpace(text)
										row = append(row, text)
									}
								}
								rows = append(rows, row)
							}
						}
					}
				}

				// Now we can process the headers and rows
				// We expect headers to be ["Name", "Version"]
				if len(headers) >= 2 {
					nameIndex := -1
					versionIndex := -1
					for i, h := range headers {
						if strings.EqualFold(h, "Name") {
							nameIndex = i
						} else if strings.EqualFold(h, "Version") {
							versionIndex = i
						}
					}
					if nameIndex == -1 || versionIndex == -1 {
						return ast.SkipChildren
					}

					for _, row := range rows {
						if len(row) >= len(headers) {
							name := row[nameIndex]
							version := row[versionIndex]
							// Clean up name and version
							name = cleanUpMarkdownName(name)
							version = strings.TrimSpace(version)
							nameVersionMap[name] = version
						}
					}
				}

				inTargetSection = false // We've processed the table, exit the section
				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})

	if len(nameVersionMap) == 0 {
		return nil, fmt.Errorf("%s section not found or empty", sectionName)
	}

	return nameVersionMap, nil
}

// cleanUpMarkdownName cleans up markdown link or anchor syntax from a name
func cleanUpMarkdownName(name string) string {
	// Remove any markdown links or anchors
	// For example, from
	// <a name="requirement_terraform"></a> [terraform](#requirement\_terraform)
	// we want to extract "terraform"
	// So we can remove anything inside <>
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "<") {
		if idx := strings.Index(name, ">"); idx != -1 {
			name = name[idx+1:]
		}
	}
	// Remove markdown link syntax
	// [terraform](...)
	if strings.HasPrefix(name, "[") {
		if idx := strings.Index(name, "]"); idx != -1 {
			name = name[1:idx]
		}
	}
	return strings.TrimSpace(name)
}

// compareRequirements compares Terraform requirements with those in the README
func compareRequirements(tfRequirements, mdRequirements map[string]string) []error {
	var errors []error

	for name, tfVersion := range tfRequirements {
		if mdVersion, ok := mdRequirements[name]; ok {
			if tfVersion != mdVersion {
				errors = append(errors, formatError("Requirement %s version mismatch:\n  Terraform: %s\n  Markdown: %s", name, tfVersion, mdVersion))
			}
		} else {
			errors = append(errors, formatError("Requirement %s is in terraform.tf but missing in README", name))
		}
	}
	for name := range mdRequirements {
		if _, ok := tfRequirements[name]; !ok {
			errors = append(errors, formatError("Requirement %s is in README but missing in terraform.tf", name))
		}
	}
	return errors
}

// compareProviders compares Terraform providers with those in the README
func compareProviders(tfProviders, mdProviders map[string]string) []error {
	var errors []error

	for name, tfVersion := range tfProviders {
		if mdVersion, ok := mdProviders[name]; ok {
			if tfVersion != mdVersion {
				errors = append(errors, formatError("Provider %s version mismatch:\n  Terraform: %s\n  Markdown: %s", name, tfVersion, mdVersion))
			}
		} else {
			errors = append(errors, formatError("Provider %s is in terraform.tf but missing in README", name))
		}
	}
	for name := range mdProviders {
		if _, ok := tfProviders[name]; !ok {
			errors = append(errors, formatError("Provider %s is in README but missing in terraform.tf", name))
		}
	}
	return errors
}

// TestMarkdown runs the markdown validation tests
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
