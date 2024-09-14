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
		NewVariableValidator(data),
		NewOutputValidator(data),
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
			text := extractText(heading)
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
						headerText := extractTextFromNodes(cell.GetChildren())
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
	return &FileValidator{files: files}
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
	if err != nil {
		if os.IsNotExist(err) {
			errors = append(errors, formatError("file does not exist:\n  %s", filePath))
		} else {
			errors = append(errors, formatError("error accessing file:\n  %s\n  %v", filePath, err))
		}
		return errors
	}

	if fileInfo.Size() == 0 {
		errors = append(errors, formatError("file is empty:\n  %s", filePath))
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

// VariableValidator validates variables in Terraform and markdown
type VariableValidator struct {
	data string
}

// NewVariableValidator creates a new VariableValidator
func NewVariableValidator(data string) *VariableValidator {
	return &VariableValidator{data: data}
}

// Validate compares Terraform variables with those documented in the markdown
func (vv *VariableValidator) Validate() []error {
	variablesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "variables.tf")
	variables, err := extractVariables(variablesPath)
	if err != nil {
		return []error{err}
	}

	markdownVariables, err := extractMarkdownVariables(vv.data)
	if err != nil {
		return []error{err}
	}

	return compareTerraformAndMarkdown(variables, markdownVariables, "Variables")
}

// OutputValidator validates outputs in Terraform and markdown
type OutputValidator struct {
	data string
}

// NewOutputValidator creates a new OutputValidator
func NewOutputValidator(data string) *OutputValidator {
	return &OutputValidator{data: data}
}

// Validate compares Terraform outputs with those documented in the markdown
func (ov *OutputValidator) Validate() []error {
	outputsPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "outputs.tf")
	outputs, err := extractOutputs(outputsPath)
	if err != nil {
		return []error{err}
	}

	markdownOutputs, err := extractMarkdownOutputs(ov.data)
	if err != nil {
		return []error{err}
	}

	return compareTerraformAndMarkdown(outputs, markdownOutputs, "Outputs")
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

// extractVariables extracts variable names from a Terraform file
func extractVariables(filePath string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	}

	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(content, filePath)
	if diags.HasErrors() {
		return nil, fmt.Errorf("error parsing HCL: %v", diags)
	}

	var variables []string
	body := file.Body
	hclContent, diags := body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "variable", LabelNames: []string{"name"}},
		},
	})
	if diags.HasErrors() {
		return nil, fmt.Errorf("error getting content: %v", diags)
	}

	for _, block := range hclContent.Blocks {
		if len(block.Labels) > 0 {
			variables = append(variables, block.Labels[0])
		}
	}

	return variables, nil
}

// extractOutputs extracts output names from a Terraform file
func extractOutputs(filePath string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	}

	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(content, filePath)
	if diags.HasErrors() {
		return nil, fmt.Errorf("error parsing HCL: %v", diags)
	}

	var outputs []string
	body := file.Body
	hclContent, diags := body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "output", LabelNames: []string{"name"}},
		},
	})
	if diags.HasErrors() {
		return nil, fmt.Errorf("error getting content: %v", diags)
	}

	for _, block := range hclContent.Blocks {
		if len(block.Labels) > 0 {
			outputs = append(outputs, block.Labels[0])
		}
	}

	return outputs, nil
}

// extractMarkdownVariables extracts variable names from the markdown
func extractMarkdownVariables(data string) ([]string, error) {
	return extractMarkdownSectionItems(data, "Inputs")
}

// extractMarkdownOutputs extracts output names from the markdown
func extractMarkdownOutputs(data string) ([]string, error) {
	return extractMarkdownSectionItems(data, "Outputs")
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
			text := extractText(heading)
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
										item = strings.Trim(item, "`") // Remove backticks if present
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
			text := extractText(heading)
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
										name = strings.Trim(name, "[]") // Remove brackets
										resourceType := extractTextFromNodes(typeCell.GetChildren())
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

// extractText extracts text from a node
func extractText(node ast.Node) string {
	var sb strings.Builder
	ast.WalkFunc(node, func(n ast.Node, entering bool) ast.WalkStatus {
		if textNode, ok := n.(*ast.Text); ok && entering {
			sb.Write(textNode.Literal)
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

	mainPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "main.tf")
	specificResources, specificDataSources, err := extractFromFilePath(mainPath)
	if err != nil {
		return nil, nil, err
	}
	resources = append(resources, specificResources...)
	dataSources = append(dataSources, specificDataSources...)

	modulesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "modules")
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
		return nil, nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	}

	parser := hclparse.NewParser()
	file, diags := parser.ParseHCL(content, filePath)
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("error parsing HCL: %v", diags)
	}

	var resources []string
	var dataSources []string

	body := file.Body
	hclContent, diags := body.Content(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: "resource", LabelNames: []string{"type", "name"}},
			{Type: "data", LabelNames: []string{"type", "name"}},
		},
	})
	if diags.HasErrors() {
		return nil, nil, fmt.Errorf("error getting content: %v", diags)
	}

	for _, block := range hclContent.Blocks {
		if len(block.Labels) >= 2 {
			resourceType := block.Labels[0]
			if block.Type == "resource" {
				resources = append(resources, resourceType)
			} else if block.Type == "data" {
				dataSources = append(dataSources, resourceType)
			}
		}
	}

	return resources, dataSources, nil
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
	//"regexp"
	//"strings"
	//"sync"
	//"testing"

	//"github.com/hashicorp/hcl/v2"
	//"github.com/hashicorp/hcl/v2/hclparse"
	//"mvdan.cc/xurls/v2"
//)

//// Validator is an interface for all validators
//type Validator interface {
	//Validate() []error
//}

//// MarkdownValidator is the main validator that orchestrates all validations
//type MarkdownValidator struct {
	//readmePath string
	//data       string
	//validators []Validator
//}

//// NewMarkdownValidator creates a new instance of MarkdownValidator
//func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
	//if envPath := os.Getenv("README_PATH"); envPath != "" {
		//readmePath = envPath
	//}

	//absReadmePath, err := filepath.Abs(readmePath)
	//if err != nil {
		//return nil, fmt.Errorf("failed to get absolute path: %v", err)
	//}

	//data, err := os.ReadFile(absReadmePath)
	//if err != nil {
		//return nil, fmt.Errorf("failed to read file: %v", err)
	//}

	//mv := &MarkdownValidator{
		//readmePath: absReadmePath,
		//data:       string(data),
	//}

	//// Initialize validators
	//mv.validators = []Validator{
		//NewSectionValidator(mv.data),
		//NewFileValidator(absReadmePath),
		//NewURLValidator(mv.data),
		//NewTerraformDefinitionValidator(mv.data),
		//NewVariableValidator(mv.data),
		//NewOutputValidator(mv.data),
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

//// Section represents a markdown section with optional table columns
//type Section struct {
	//Header  string
	//Columns []string
//}

//// SectionValidator validates sections in the markdown
//type SectionValidator struct {
	//data     string
	//sections []Section
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
	//return &SectionValidator{
		//data:     data,
		//sections: sections,
	//}
//}

//// Validate validates the sections in the markdown
//func (sv *SectionValidator) Validate() []error {
	//var allErrors []error
	//for _, section := range sv.sections {
		//allErrors = append(allErrors, section.validate(sv.data)...)
	//}
	//return allErrors
//}

//// validate checks if a section and its columns are correctly formatted
//func (s Section) validate(data string) []error {
	//var errors []error
	//tableHeaderRegex := `^\s*\|(.+?)\|\s*(\r?\n)`

	//flexibleHeaderPattern := regexp.MustCompile(`(?mi)^\s*##\s+` + regexp.QuoteMeta(s.Header) + `s?\s*$`)
	//headerLoc := flexibleHeaderPattern.FindStringIndex(data)

	//if headerLoc == nil {
		//errors = append(errors, compareHeaders(s.Header, ""))
	//} else {
		//actualHeader := strings.TrimSpace(data[headerLoc[0]:headerLoc[1]])
		//if actualHeader != "## "+s.Header {
			//errors = append(errors, compareHeaders(s.Header, actualHeader[3:])) // Remove "## " prefix
		//}

		//if len(s.Columns) > 0 {
			//startIdx := headerLoc[1]
			//dataSlice := data[startIdx:]

			//tableHeaderPattern := regexp.MustCompile(tableHeaderRegex)
			//tableHeaderMatch := tableHeaderPattern.FindStringSubmatch(dataSlice)
			//if tableHeaderMatch == nil {
				//errors = append(errors, formatError("missing table after header: %s", actualHeader))
			//} else {
				//actualHeaders := parseHeaders(tableHeaderMatch[1])
				//if !equalSlices(actualHeaders, s.Columns) {
					//errors = append(errors, compareColumns(s.Header, s.Columns, actualHeaders))
				//}
			//}
		//}
	//}

	//return errors
//}

//// FileValidator validates the presence and content of required files
//type FileValidator struct {
	//files []string
//}

//// NewFileValidator creates a new FileValidator
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
	//return &FileValidator{files: files}
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
	//if err != nil {
		//if os.IsNotExist(err) {
			//errors = append(errors, formatError("file does not exist:\n  %s", filePath))
		//} else {
			//errors = append(errors, formatError("error accessing file:\n  %s\n  %v", filePath, err))
		//}
		//return errors
	//}

	//if fileInfo.Size() == 0 {
		//errors = append(errors, formatError("file is empty:\n  %s", filePath))
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

//// VariableValidator validates variables in Terraform and markdown
//type VariableValidator struct {
	//data string
//}

//// NewVariableValidator creates a new VariableValidator
//func NewVariableValidator(data string) *VariableValidator {
	//return &VariableValidator{data: data}
//}

//// Validate compares Terraform variables with those documented in the markdown
//func (vv *VariableValidator) Validate() []error {
	//variablesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "variables.tf")
	//variables, err := extractVariables(variablesPath)
	//if err != nil {
		//return []error{err}
	//}

	//markdownVariables, err := extractMarkdownVariables(vv.data)
	//if err != nil {
		//return []error{err}
	//}

	//return compareTerraformAndMarkdown(variables, markdownVariables, "Variables")
//}

//// OutputValidator validates outputs in Terraform and markdown
//type OutputValidator struct {
	//data string
//}

//// NewOutputValidator creates a new OutputValidator
//func NewOutputValidator(data string) *OutputValidator {
	//return &OutputValidator{data: data}
//}

//// Validate compares Terraform outputs with those documented in the markdown
//func (ov *OutputValidator) Validate() []error {
	//outputsPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "outputs.tf")
	//outputs, err := extractOutputs(outputsPath)
	//if err != nil {
		//return []error{err}
	//}

	//markdownOutputs, err := extractMarkdownOutputs(ov.data)
	//if err != nil {
		//return []error{err}
	//}

	//return compareTerraformAndMarkdown(outputs, markdownOutputs, "Outputs")
//}

//// compareHeaders compares expected and actual headers and returns an error if they don't match
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

//// parseHeaders parses table headers from a markdown table
//func parseHeaders(headerRow string) []string {
	//headers := strings.Split(strings.TrimSpace(headerRow), "|")
	//for i, header := range headers {
		//headers[i] = strings.TrimSpace(header)
	//}
	//return headers
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

//// compareColumns compares expected and actual table columns and returns an error if they don't match
//func compareColumns(header string, expected, actual []string) error {
	//var mismatchedColumns []string
	//for i := 0; i < len(expected) || i < len(actual); i++ {
		//expectedCol := ""
		//actualCol := ""
		//if i < len(expected) {
			//expectedCol = expected[i]
		//}
		//if i < len(actual) {
			//actualCol = actual[i]
		//}
		//if expectedCol != actualCol {
			//mismatchedColumns = append(mismatchedColumns, fmt.Sprintf("expected '%s', found '%s'", expectedCol, actualCol))
		//}
	//}
	//return formatError("table under header: %s has incorrect column names:\n  %s", header, strings.Join(mismatchedColumns, "\n  "))
//}

//// findMissingItems finds items in slice a that are not in slice b
//func findMissingItems(a, b []string) []string {
	//mb := make(map[string]struct{}, len(b))
	//for _, x := range b {
		//mb[x] = struct{}{}
	//}
	//var diff []string
	//for _, x := range a {
		//if _, found := mb[x]; !found {
			//diff = append(diff, x)
		//}
	//}
	//return diff
//}

//// compareTerraformAndMarkdown compares items in Terraform and markdown and returns errors for discrepancies
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

//// extractVariables extracts variable names from a Terraform file
//func extractVariables(filePath string) ([]string, error) {
	//content, err := os.ReadFile(filePath)
	//if err != nil {
		//return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	//}

	//parser := hclparse.NewParser()
	//file, diags := parser.ParseHCL(content, filePath)
	//if diags.HasErrors() {
		//return nil, fmt.Errorf("error parsing HCL: %v", diags)
	//}

	//var variables []string
	//body := file.Body
	//hclContent, diags := body.Content(&hcl.BodySchema{
		//Blocks: []hcl.BlockHeaderSchema{
			//{Type: "variable", LabelNames: []string{"name"}},
		//},
	//})
	//if diags.HasErrors() {
		//return nil, fmt.Errorf("error getting content: %v", diags)
	//}

	//for _, block := range hclContent.Blocks {
		//if block.Type == "variable" {
			//if len(block.Labels) == 0 {
				//return nil, fmt.Errorf("variable block without a name at %s", block.TypeRange.String())
			//}
			//variables = append(variables, block.Labels[0])
		//}
	//}

	//if len(variables) == 0 {
		//return nil, fmt.Errorf("no variables found in %s", filePath)
	//}

	//return variables, nil
//}

//// extractOutputs extracts output names from a Terraform file
//func extractOutputs(filePath string) ([]string, error) {
	//content, err := os.ReadFile(filePath)
	//if err != nil {
		//return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	//}

	//parser := hclparse.NewParser()
	//file, diags := parser.ParseHCL(content, filePath)
	//if diags.HasErrors() {
		//return nil, fmt.Errorf("error parsing HCL: %v", diags)
	//}

	//var outputs []string
	//body := file.Body
	//hclContent, diags := body.Content(&hcl.BodySchema{
		//Blocks: []hcl.BlockHeaderSchema{
			//{Type: "output", LabelNames: []string{"name"}},
		//},
	//})
	//if diags.HasErrors() {
		//return nil, fmt.Errorf("error getting content: %v", diags)
	//}

	//for _, block := range hclContent.Blocks {
		//if block.Type == "output" {
			//if len(block.Labels) == 0 {
				//return nil, fmt.Errorf("output block without a name at %s", block.TypeRange.String())
			//}
			//outputs = append(outputs, block.Labels[0])
		//}
	//}

	//if len(outputs) == 0 {
		//return nil, fmt.Errorf("no outputs found in %s", filePath)
	//}

	//return outputs, nil
//}

//// extractMarkdownVariables extracts variable names from the markdown
//func extractMarkdownVariables(data string) ([]string, error) {
	//return extractMarkdownSection(data, "Inputs")
//}

//// extractMarkdownOutputs extracts output names from the markdown
//func extractMarkdownOutputs(data string) ([]string, error) {
	//return extractMarkdownSection(data, "Outputs")
//}

//// extractMarkdownSection extracts items from a markdown section
//func extractMarkdownSection(data, sectionName string) ([]string, error) {
	//var items []string
	//sectionPattern := regexp.MustCompile(`(?s)## ` + sectionName + `.*?\n(.*?)\n##`)
	//sectionContent := sectionPattern.FindStringSubmatch(data)
	//if len(sectionContent) < 2 {
		//return nil, fmt.Errorf("%s section not found or empty", sectionName)
	//}

	//linePattern := regexp.MustCompile(`\|\s*` + "`([^`]+)`" + `\s*\|`)
	//matches := linePattern.FindAllStringSubmatch(sectionContent[1], -1)

	//for _, match := range matches {
		//if len(match) > 1 {
			//items = append(items, strings.TrimSpace(match[1]))
		//}
	//}

	//return items, nil
//}

//// extractReadmeResources extracts resources and data sources from the markdown
//func extractReadmeResources(data string) ([]string, []string, error) {
	//var resources []string
	//var dataSources []string
	//resourcesPattern := regexp.MustCompile(`(?s)## Resources.*?\n(.*?)\n##`)
	//resourcesSection := resourcesPattern.FindStringSubmatch(data)
	//if len(resourcesSection) < 2 {
		//return nil, nil, errors.New("resources section not found or empty")
	//}

	//linePattern := regexp.MustCompile(`\|\s*\[([^\]]+)\].*\|\s*(resource|data source)\s*\|`)
	//matches := linePattern.FindAllStringSubmatch(resourcesSection[1], -1)

	//for _, match := range matches {
		//if len(match) > 2 {
			//if match[2] == "resource" {
				//resources = append(resources, strings.TrimSpace(match[1]))
			//} else if match[2] == "data source" {
				//dataSources = append(dataSources, strings.TrimSpace(match[1]))
			//}
		//}
	//}

	//return resources, dataSources, nil
//}

//// extractTerraformResources extracts resources and data sources from Terraform files
//func extractTerraformResources() ([]string, []string, error) {
	//var resources []string
	//var dataSources []string

	//mainPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "main.tf")
	//specificResources, specificDataSources, err := extractFromFilePath(mainPath)
	//if err != nil {
		//return nil, nil, err
	//}
	//resources = append(resources, specificResources...)
	//dataSources = append(dataSources, specificDataSources...)

	//modulesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "modules")
	//modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
	//if err != nil {
		//return nil, nil, err
	//}
	//resources = append(resources, modulesResources...)
	//dataSources = append(dataSources, modulesDataSources...)

	//return resources, dataSources, nil
//}

//// extractRecursively extracts resources and data sources from Terraform files recursively
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
		//if info.Mode().IsRegular() && filepath.Base(path) == "main.tf" {
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
		//return nil, nil, fmt.Errorf("error reading file %s: %v", filePath, err)
	//}

	//var resources []string
	//var dataSources []string

	//resourceRegex := regexp.MustCompile(`(?m)^\s*resource\s+"(\w+)"\s+"`)
	//dataRegex := regexp.MustCompile(`(?m)^\s*data\s+"(\w+)"\s+"`)

	//for _, match := range resourceRegex.FindAllStringSubmatch(string(content), -1) {
		//resources = append(resources, match[1])
	//}

	//for _, match := range dataRegex.FindAllStringSubmatch(string(content), -1) {
		//dataSources = append(dataSources, match[1])
	//}

	//return resources, dataSources, nil
//}

//// main function is required for package main, but we're focusing on tests
//func main() {
	//// Placeholder main function
//}

//// TestMarkdown is the test function
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

//package main

//import (
//"errors"
//"fmt"
//"net/http"
//"os"
//"path/filepath"
//"regexp"
//"strings"
//"sync"
//"testing"

//"github.com/hashicorp/hcl/v2"
//"github.com/hashicorp/hcl/v2/hclparse"
//"mvdan.cc/xurls/v2"
//)

//type Validator interface {
//Validate() []error
//}

//type SectionValidator interface {
//ValidateSection(data string) []error
//}

//type FileValidator interface {
//ValidateFile(filePath string) []error
//}

//type URLValidator interface {
//ValidateURLs(data string) []error
//}

//type TerraformValidator interface {
//ValidateTerraformDefinitions(data string) []error
//}

//type VariableValidator interface {
//ValidateVariables(data string) []error
//}

//type OutputValidator interface {
//ValidateOutputs(data string) []error
//}

//type MarkdownValidator struct {
//readmePath        string
//data              string
//sections          []SectionValidator
//files             []FileValidator
//urlValidator      URLValidator
//tfValidator       TerraformValidator
//variableValidator VariableValidator
//outputValidator   OutputValidator
//}

//type Section struct {
//Header  string
//Columns []string
//}

//type RequiredFile struct {
//Name string
//}

//type StandardURLValidator struct{}

//type TerraformDefinitionValidator struct{}

//type StandardVariableValidator struct{}

//type StandardOutputValidator struct{}

//type TerraformConfig struct {
//Resource []Resource `hcl:"resource,block"`
//Data     []Data     `hcl:"data,block"`
//}

//type Resource struct {
//Type       string   `hcl:"type,label"`
//Name       string   `hcl:"name,label"`
//Properties hcl.Body `hcl:",remain"`
//}

//type Data struct {
//Type       string   `hcl:"type,label"`
//Name       string   `hcl:"name,label"`
//Properties hcl.Body `hcl:",remain"`
//}

//func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
//if envPath := os.Getenv("README_PATH"); envPath != "" {
//readmePath = envPath
//}

//absReadmePath, err := filepath.Abs(readmePath)
//if err != nil {
//return nil, fmt.Errorf("failed to get absolute path: %v", err)
//}

//data, err := os.ReadFile(absReadmePath)
//if err != nil {
//return nil, fmt.Errorf("failed to read file: %v", err)
//}

//sections := []SectionValidator{
//Section{Header: "Goals"},
//Section{Header: "Resources", Columns: []string{"Name", "Type"}},
//Section{Header: "Providers", Columns: []string{"Name", "Version"}},
//Section{Header: "Requirements", Columns: []string{"Name", "Version"}},
//Section{Header: "Inputs", Columns: []string{"Name", "Description", "Type", "Required"}},
//Section{Header: "Outputs", Columns: []string{"Name", "Description"}},
//Section{Header: "Features"},
//Section{Header: "Testing"},
//Section{Header: "Authors"},
//Section{Header: "License"},
//}

//rootDir := filepath.Dir(absReadmePath)

//files := []FileValidator{
//RequiredFile{Name: absReadmePath},
//RequiredFile{Name: filepath.Join(rootDir, "CONTRIBUTING.md")},
//RequiredFile{Name: filepath.Join(rootDir, "CODE_OF_CONDUCT.md")},
//RequiredFile{Name: filepath.Join(rootDir, "SECURITY.md")},
//RequiredFile{Name: filepath.Join(rootDir, "LICENSE")},
//RequiredFile{Name: filepath.Join(rootDir, "outputs.tf")},
//RequiredFile{Name: filepath.Join(rootDir, "variables.tf")},
//RequiredFile{Name: filepath.Join(rootDir, "terraform.tf")},
//RequiredFile{Name: filepath.Join(rootDir, "Makefile")},
//}

//return &MarkdownValidator{
//readmePath:        absReadmePath,
//data:              string(data),
//sections:          sections,
//files:             files,
//urlValidator:      StandardURLValidator{},
//tfValidator:       TerraformDefinitionValidator{},
//variableValidator: StandardVariableValidator{},
//outputValidator:   StandardOutputValidator{},
//}, nil
//}

//func (mv *MarkdownValidator) Validate() []error {
//var allErrors []error

//allErrors = append(allErrors, mv.ValidateSections()...)
//allErrors = append(allErrors, mv.ValidateFiles()...)
//allErrors = append(allErrors, mv.ValidateURLs()...)
//allErrors = append(allErrors, mv.ValidateTerraformDefinitions()...)
//allErrors = append(allErrors, mv.ValidateVariables()...)
//allErrors = append(allErrors, mv.ValidateOutputs()...)

//return allErrors
//}

//func (mv *MarkdownValidator) ValidateSections() []error {
//var allErrors []error
//for _, section := range mv.sections {
//allErrors = append(allErrors, section.ValidateSection(mv.data)...)
//}
//return allErrors
//}

//func (s Section) ValidateSection(data string) []error {
//var errors []error
//tableHeaderRegex := `^\s*\|(.+?)\|\s*(\r?\n)`

//flexibleHeaderPattern := regexp.MustCompile(`(?mi)^\s*##\s+` + strings.Replace(regexp.QuoteMeta(s.Header), `\s+`, `\s+`, -1) + `s?\s*$`)
//headerLoc := flexibleHeaderPattern.FindStringIndex(data)

//if headerLoc == nil {
//errors = append(errors, compareHeaders(s.Header, ""))
//} else {
//actualHeader := strings.TrimSpace(data[headerLoc[0]:headerLoc[1]])
//if actualHeader != "## "+s.Header {
//errors = append(errors, compareHeaders(s.Header, actualHeader[3:])) // Remove "## " prefix
//}

//if len(s.Columns) > 0 {
//startIdx := headerLoc[1]
//dataSlice := data[startIdx:]

//tableHeaderPattern := regexp.MustCompile(tableHeaderRegex)
//tableHeaderMatch := tableHeaderPattern.FindStringSubmatch(dataSlice)
//if tableHeaderMatch == nil {
//errors = append(errors, formatError("missing table after header: %s", actualHeader))
//} else {
//actualHeaders := parseHeaders(tableHeaderMatch[1])
//if !equalSlices(actualHeaders, s.Columns) {
//errors = append(errors, compareColumns(s.Header, s.Columns, actualHeaders))
//}
//}
//}
//}

//return errors
//}

//func compareHeaders(expected, actual string) error {
//var mismatchedHeaders []string
//if expected != actual {
//if actual == "" {
//mismatchedHeaders = append(mismatchedHeaders, fmt.Sprintf("expected '%s', found 'not present'", expected))
//} else {
//mismatchedHeaders = append(mismatchedHeaders, fmt.Sprintf("expected '%s', found '%s'", expected, actual))
//}
//}
//return formatError("incorrect header:\n  %s", strings.Join(mismatchedHeaders, "\n  "))
//}

//func (mv *MarkdownValidator) ValidateFiles() []error {
//var allErrors []error
//for _, file := range mv.files {
//allErrors = append(allErrors, file.ValidateFile(file.(RequiredFile).Name)...)
//}
//return allErrors
//}

//func (rf RequiredFile) ValidateFile(filePath string) []error {
//var errors []error
//fileInfo, err := os.Stat(filePath)
//if err != nil {
//if os.IsNotExist(err) {
//errors = append(errors, formatError("file does not exist:\n  %s", filePath))
//} else {
//errors = append(errors, formatError("error accessing file:\n  %s\n  %v", filePath, err))
//}
//return errors
//}

//if fileInfo.Size() == 0 {
//errors = append(errors, formatError("file is empty:\n  %s", filePath))
//}

//return errors
//}

//func (mv *MarkdownValidator) ValidateURLs() []error {
//return mv.urlValidator.ValidateURLs(mv.data)
//}

//func (suv StandardURLValidator) ValidateURLs(data string) []error {
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
//if err := suv.validateSingleURL(url); err != nil {
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

//func (suv StandardURLValidator) validateSingleURL(url string) error {
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

//func (mv *MarkdownValidator) ValidateTerraformDefinitions() []error {
//return mv.tfValidator.ValidateTerraformDefinitions(mv.data)
//}

//func (tdv TerraformDefinitionValidator) ValidateTerraformDefinitions(data string) []error {
//tfResources, tfDataSources, err := extractTerraformResources()
//if err != nil {
//return []error{err}
//}

//readmeResources, readmeDataSources, err := extractReadmeResources(data)
//if err != nil {
//return []error{err}
//}

//var errors []error
//errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
//errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)

//return errors
//}

//func (mv *MarkdownValidator) ValidateVariables() []error {
//return mv.variableValidator.ValidateVariables(mv.data)
//}

//func (svv StandardVariableValidator) ValidateVariables(data string) []error {
//variablesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "variables.tf")
//variables, err := extractVariables(variablesPath)
//if err != nil {
//return []error{err}
//}

//markdownVariables, err := extractMarkdownVariables(data)
//if err != nil {
//return []error{err}
//}

//return compareTerraformAndMarkdown(variables, markdownVariables, "Variables")
//}

//func (mv *MarkdownValidator) ValidateOutputs() []error {
//return mv.outputValidator.ValidateOutputs(mv.data)
//}

//func (sov StandardOutputValidator) ValidateOutputs(data string) []error {
//outputsPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "outputs.tf")
//outputs, err := extractOutputs(outputsPath)
//if err != nil {
//return []error{err}
//}

//markdownOutputs, err := extractMarkdownOutputs(data)
//if err != nil {
//return []error{err}
//}

//return compareTerraformAndMarkdown(outputs, markdownOutputs, "Outputs")
//}

//func extractVariables(filePath string) ([]string, error) {
//content, err := os.ReadFile(filePath)
//if err != nil {
//return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
//}

//parser := hclparse.NewParser()
//file, diags := parser.ParseHCL(content, filePath)
//if diags.HasErrors() {
//return nil, fmt.Errorf("error parsing HCL: %v", diags)
//}

//var variables []string
//body := file.Body
//hclContent, diags := body.Content(&hcl.BodySchema{
//Blocks: []hcl.BlockHeaderSchema{
//{Type: "variable", LabelNames: []string{"name"}},
//},
//})
//if diags.HasErrors() {
//return nil, fmt.Errorf("error getting content: %v", diags)
//}

//for _, block := range hclContent.Blocks {
//if block.Type == "variable" {
//if len(block.Labels) == 0 {
//return nil, fmt.Errorf("variable block without a name at %s", block.TypeRange.String())
//}
//variables = append(variables, block.Labels[0])
//}
//}

//if len(variables) == 0 {
//return nil, fmt.Errorf("no variables found in %s", filePath)
//}

//return variables, nil
//}

//func extractOutputs(filePath string) ([]string, error) {
//content, err := os.ReadFile(filePath)
//if err != nil {
//return nil, fmt.Errorf("error reading file %s: %v", filePath, err)
//}

//parser := hclparse.NewParser()
//file, diags := parser.ParseHCL(content, filePath)
//if diags.HasErrors() {
//return nil, fmt.Errorf("error parsing HCL: %v", diags)
//}

//var outputs []string
//body := file.Body
//hclContent, diags := body.Content(&hcl.BodySchema{
//Blocks: []hcl.BlockHeaderSchema{
//{Type: "output", LabelNames: []string{"name"}},
//},
//})
//if diags.HasErrors() {
//return nil, fmt.Errorf("error getting content: %v", diags)
//}

//for _, block := range hclContent.Blocks {
//if block.Type == "output" {
//if len(block.Labels) == 0 {
//return nil, fmt.Errorf("output block without a name at %s", block.TypeRange.String())
//}
//outputs = append(outputs, block.Labels[0])
//}
//}

//if len(outputs) == 0 {
//return nil, fmt.Errorf("no outputs found in %s", filePath)
//}

//return outputs, nil
//}

//func extractMarkdownVariables(data string) ([]string, error) {
//return extractMarkdownSection(data, "Inputs")
//}

//func extractMarkdownOutputs(data string) ([]string, error) {
//return extractMarkdownSection(data, "Outputs")
//}

//func extractReadmeResources(data string) ([]string, []string, error) {
//var resources []string
//var dataSources []string
//resourcesPattern := regexp.MustCompile(`(?s)## Resources.*?\n(.*?)\n##`)
//resourcesSection := resourcesPattern.FindStringSubmatch(data)
//if len(resourcesSection) < 2 {
//return nil, nil, errors.New("resources section not found or empty")
//}

//linePattern := regexp.MustCompile(`\|\s*\[([^\]]+)\].*\|\s*(resource|data source)\s*\|`)
//matches := linePattern.FindAllStringSubmatch(resourcesSection[1], -1)

//for _, match := range matches {
//if len(match) > 2 {
//if match[2] == "resource" {
//resources = append(resources, strings.TrimSpace(match[1]))
//} else if match[2] == "data source" {
//dataSources = append(dataSources, strings.TrimSpace(match[1]))
//}
//}
//}

//return resources, dataSources, nil
//}

//func extractTerraformResources() ([]string, []string, error) {
//var resources []string
//var dataSources []string

//mainPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "main.tf")
//specificResources, specificDataSources, err := extractFromFilePath(mainPath)
//if err != nil {
//return nil, nil, err
//}
//resources = append(resources, specificResources...)
//dataSources = append(dataSources, specificDataSources...)

//modulesPath := filepath.Join(os.Getenv("GITHUB_WORKSPACE"), "caller", "modules")
//modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
//if err != nil {
//return nil, nil, err
//}
//resources = append(resources, modulesResources...)
//dataSources = append(dataSources, modulesDataSources...)

//return resources, dataSources, nil
//}

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
//if info.Mode().IsRegular() && filepath.Base(path) == "main.tf" {
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

//func extractFromFilePath(filePath string) ([]string, []string, error) {
//content, err := os.ReadFile(filePath)
//if err != nil {
//return nil, nil, fmt.Errorf("error reading file %s: %v", filePath, err)
//}

//var resources []string
//var dataSources []string

//resourceRegex := regexp.MustCompile(`(?m)^resource\s+"(\w+)"\s+"`)
//dataRegex := regexp.MustCompile(`(?m)^data\s+"(\w+)"\s+"`)

//for _, match := range resourceRegex.FindAllStringSubmatch(string(content), -1) {
//resources = append(resources, match[1])
//}

//for _, match := range dataRegex.FindAllStringSubmatch(string(content), -1) {
//dataSources = append(dataSources, match[1])
//}

//return resources, dataSources, nil
//}

//func formatError(format string, args ...interface{}) error {
//return fmt.Errorf(format, args...)
//}

//func parseHeaders(headerRow string) []string {
//headers := strings.Split(strings.TrimSpace(headerRow), "|")
//for i, header := range headers {
//headers[i] = strings.TrimSpace(header)
//}
//return headers
//}

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

//func findMissingItems(a, b []string) []string {
//mb := make(map[string]struct{}, len(b))
//for _, x := range b {
//mb[x] = struct{}{}
//}
//var diff []string
//for _, x := range a {
//if _, found := mb[x]; !found {
//diff = append(diff, x)
//}
//}
//return diff
//}

//func compareColumns(header string, expected, actual []string) error {
//var mismatchedColumns []string
//for i := 0; i < len(expected) || i < len(actual); i++ {
//expectedCol := ""
//actualCol := ""
//if i < len(expected) {
//expectedCol = expected[i]
//}
//if i < len(actual) {
//actualCol = actual[i]
//}
//if expectedCol != actualCol {
//mismatchedColumns = append(mismatchedColumns, fmt.Sprintf("expected '%s', found '%s'", expectedCol, actualCol))
//}
//}
//return formatError("table under header: %s has incorrect column names:\n  %s", header, strings.Join(mismatchedColumns, "\n  "))
//}

//func extractMarkdownSection(data, sectionName string) ([]string, error) {
//var items []string
//sectionPattern := regexp.MustCompile(`(?s)## ` + sectionName + `.*?\n(.*?)\n##`)
//sectionContent := sectionPattern.FindStringSubmatch(data)
//if len(sectionContent) < 2 {
//return nil, fmt.Errorf("%s section not found or empty", sectionName)
//}

//linePattern := regexp.MustCompile(`\|\s*` + "`([^`]+)`" + `\s*\|`)
//matches := linePattern.FindAllStringSubmatch(sectionContent[1], -1)

//for _, match := range matches {
//if len(match) > 1 {
//items = append(items, strings.TrimSpace(match[1]))
//}
//}

//return items, nil
//}

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
