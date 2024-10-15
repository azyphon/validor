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
	Header string
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
		{Header: "Non Goals"},
		{Header: "Resources"},
		{Header: "Providers"},
		{Header: "Requirements"},
		{Header: "Optional Inputs"},
		{Header: "Required Inputs"},
		{Header: "Outputs"},
		{Header: "Testing"},
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

// Validate validates the sections in the markdown
func (sv *SectionValidator) Validate() []error {
	var allErrors []error
	for _, section := range sv.sections {
		allErrors = append(allErrors, section.validate(sv.rootNode)...)
	}
	return allErrors
}

// validate checks if a section is present
func (s Section) validate(rootNode ast.Node) []error {
	var errors []error
	found := false

	ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, s.Header) ||
				strings.EqualFold(text, s.Header+"s") ||
				(s.Header == "Inputs" && (strings.EqualFold(text, "Required Inputs") || strings.EqualFold(text, "Optional Inputs"))) {
				found = true
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

type FileValidator struct {
	files []string
}

func NewFileValidator(readmePath string) *FileValidator {
	rootDir := filepath.Dir(readmePath)
	files := []string{
		readmePath,
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

    mdItems, err := extractMarkdownSectionItems(iv.data, iv.section)
    if err != nil {
        return []error{err}
    }

    return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
}

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

    //if len(tfItems) > 0 && len(mdItems) == 0 {
        //return []error{fmt.Errorf("%s section in markdown is empty but Terraform has items", iv.section)}
    //}

    //return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
//}

// Validate compares Terraform items with those documented in the markdown
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

	hclContent, _, _ := body.PartialContent(&hcl.BodySchema{
		Blocks: []hcl.BlockHeaderSchema{
			{Type: blockType, LabelNames: []string{"name"}},
		},
	})

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
			if block.Type == "resource" {
				resources = append(resources, resourceType)
				resources = append(resources, fullResourceName)
			} else if block.Type == "data" {
				dataSources = append(dataSources, resourceType)
				dataSources = append(dataSources, fullResourceName)
			}
		}
	}

	return resources, dataSources, nil
}

func extractMarkdownSectionItems(data, sectionName string) ([]string, error) {
    extensions := parser.CommonExtensions | parser.AutoHeadingIDs
    p := parser.NewWithExtensions(extensions)
    rootNode := p.Parse([]byte(data))

    var items []string
    inTargetSection := false
    sectionFound := false

    ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
        if heading, ok := n.(*ast.Heading); ok && entering {
            headingText := strings.TrimSpace(extractText(heading))
            if heading.Level == 2 {
                if strings.EqualFold(headingText, sectionName) ||
                   strings.EqualFold(headingText, "Required "+sectionName) ||
                   strings.EqualFold(headingText, "Optional "+sectionName) {
                    inTargetSection = true
                    sectionFound = true
                } else if inTargetSection {
                    inTargetSection = false
                }
            } else if heading.Level == 3 && inTargetSection {
                inputName := strings.Trim(headingText, " []")
                items = append(items, inputName)
            }
        }
        return ast.GoToNext
    })

    if !sectionFound {
        return nil, fmt.Errorf("%s section not found", sectionName)
    }

    return items, nil
}

//func extractMarkdownSectionItems(data, sectionName string) ([]string, error) {
    //extensions := parser.CommonExtensions | parser.AutoHeadingIDs
    //p := parser.NewWithExtensions(extensions)
    //rootNode := p.Parse([]byte(data))

    //var items []string
    //inTargetSection := false
    //sectionFound := false

    //ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
        //if heading, ok := n.(*ast.Heading); ok && entering {
            //headingText := strings.TrimSpace(extractText(heading))
            //if heading.Level == 2 {
                //if strings.EqualFold(headingText, sectionName) ||
                   //strings.EqualFold(headingText, "Required "+sectionName) ||
                   //strings.EqualFold(headingText, "Optional "+sectionName) {
                    //inTargetSection = true
                    //sectionFound = true
                //} else {
                    //inTargetSection = false
                //}
            //} else if heading.Level == 3 && inTargetSection {
                //inputName := strings.Trim(headingText, " []")
                //items = append(items, inputName)
            //}
        //}
        //return ast.GoToNext
    //})

    //if !sectionFound {
        //return nil, fmt.Errorf("%s section not found", sectionName)
    //}

    //return items, nil
//}

// Update the extractMarkdownSectionItems function
//func extractMarkdownSectionItems(data, sectionName string) ([]string, error) {
	//extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	//p := parser.NewWithExtensions(extensions)
	//rootNode := p.Parse([]byte(data))

	//var items []string
	//currentSection := ""

	//ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
		//if heading, ok := n.(*ast.Heading); ok && entering {
			//headingText := strings.TrimSpace(extractText(heading))
			//if heading.Level == 2 {
				//if strings.EqualFold(headingText, sectionName) ||
					//strings.EqualFold(headingText, "Required "+sectionName) ||
					//strings.EqualFold(headingText, "Optional "+sectionName) {
					//currentSection = headingText
				//} else {
					//currentSection = ""
				//}
			//} else if heading.Level == 3 && strings.Contains(currentSection, sectionName) {
				//inputName := strings.Trim(headingText, " []")
				//items = append(items, inputName)
			//}
		//}
		//return ast.GoToNext
	//})

	//if len(items) == 0 {
		//return nil, fmt.Errorf("%s section not found or empty", sectionName)
	//}

	//return items, nil
//}

func extractReadmeResources(data string) ([]string, []string, error) {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := p.Parse([]byte(data))

	var resources []string
	var dataSources []string
	inResourceSection := false

	ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := n.(*ast.Heading); ok && entering {
			headingText := extractText(heading)
			if strings.Contains(headingText, "Resources") {
				inResourceSection = true
			} else if heading.Level <= 2 {
				inResourceSection = false
			}
		}

		if inResourceSection && entering {
			if link, ok := n.(*ast.Link); ok {
				linkText := extractText(link)
				if strings.Contains(linkText, "azurerm_") {
					resourceName := strings.Split(linkText, "]")[0]
					resourceName = strings.TrimPrefix(resourceName, "[")
					resources = append(resources, resourceName)

					// Also add the base resource type without the symbolic name
					baseName := strings.Split(resourceName, ".")[0]
					if baseName != resourceName {
						resources = append(resources, baseName)
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

func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
	var errors []error
	tfSet := make(map[string]bool)
	mdSet := make(map[string]bool)
	reported := make(map[string]bool)

	// Helper function to get the full resource name
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
			errors = append(errors, formatError("%s in Terraform but missing in markdown: %s", itemType, fullName))
			reported[baseName] = true
		}
	}

	for _, mdItem := range mdItems {
		baseName := strings.Split(mdItem, ".")[0]
		if !tfSet[mdItem] && !tfSet[baseName] && !reported[baseName] {
			fullName := getFullName(mdItems, baseName)
			errors = append(errors, formatError("%s in markdown but missing in Terraform: %s", itemType, fullName))
			reported[baseName] = true
		}
	}

	return errors
}

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
//Header string
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
//{Header: "Resources"},
//{Header: "Providers"},
//{Header: "Requirements"},
//{Header: "Inputs"},
//{Header: "Outputs"},
//{Header: "Testing"},
////{Header: "Goals"},
////{Header: "Resources"},
////{Header: "Providers"},
////{Header: "Requirements"},
////{Header: "Inputs"},
////{Header: "Optional Inputs"},
////{Header: "Outputs"},
////{Header: "Testing"},
//}

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

//// validate checks if a section is present
//func (s Section) validate(rootNode ast.Node) []error {
//var errors []error
//found := false

//ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
//if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
//text := strings.TrimSpace(extractText(heading))
//if strings.EqualFold(text, s.Header) ||
//strings.EqualFold(text, s.Header+"s") ||
//(s.Header == "Inputs" && (strings.EqualFold(text, "Required Inputs") || strings.EqualFold(text, "Optional Inputs"))) {
//found = true
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
////func (s Section) validate(rootNode ast.Node) []error {
////var errors []error
////found := false

////ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
////if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
////text := strings.TrimSpace(extractText(heading))
////if strings.EqualFold(text, s.Header) || strings.EqualFold(text, s.Header+"s") {
////found = true
////return ast.SkipChildren
////}
////}
////return ast.GoToNext
////})

////if !found {
////errors = append(errors, compareHeaders(s.Header, ""))
////}

////return errors
////}

//// FileValidator validates the presence of required files
//type FileValidator struct {
//files []string
//}

//func NewFileValidator(readmePath string) *FileValidator {
//rootDir := filepath.Dir(readmePath)
//files := []string{
//readmePath,
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

//hclContent, _, _ := body.PartialContent(&hcl.BodySchema{
//Blocks: []hcl.BlockHeaderSchema{
//{Type: blockType, LabelNames: []string{"name"}},
//},
//})

//if hclContent == nil {
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

//hclContent, _, diags := body.PartialContent(&hcl.BodySchema{
//Blocks: []hcl.BlockHeaderSchema{
//{Type: "resource", LabelNames: []string{"type", "name"}},
//{Type: "data", LabelNames: []string{"type", "name"}},
//},
//})

//if diags.HasErrors() {
//return nil, nil, fmt.Errorf("error getting content from %s: %v", filepath.Base(filePath), diags)
//}

//if hclContent == nil {
//return resources, dataSources, nil
//}

//for _, block := range hclContent.Blocks {
//if len(block.Labels) >= 2 {
//resourceType := strings.TrimSpace(block.Labels[0])
//resourceName := strings.TrimSpace(block.Labels[1])
//fullResourceName := resourceType + "." + resourceName
//if block.Type == "resource" {
//resources = append(resources, resourceType)
//resources = append(resources, fullResourceName)
//} else if block.Type == "data" {
//dataSources = append(dataSources, resourceType)
//dataSources = append(dataSources, fullResourceName)
//}
//}
//}

//return resources, dataSources, nil
//}

//// Update the extractMarkdownSectionItems function
//func extractMarkdownSectionItems(data, sectionName string) ([]string, error) {
//extensions := parser.CommonExtensions | parser.AutoHeadingIDs
//p := parser.NewWithExtensions(extensions)
//rootNode := p.Parse([]byte(data))

//var items []string
//currentSection := ""

//ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
//if heading, ok := n.(*ast.Heading); ok && entering {
//headingText := strings.TrimSpace(extractText(heading))
//if heading.Level == 2 {
//if strings.EqualFold(headingText, sectionName) ||
//strings.EqualFold(headingText, "Required "+sectionName) ||
//strings.EqualFold(headingText, "Optional "+sectionName) {
//currentSection = headingText
//} else {
//currentSection = ""
//}
//} else if heading.Level == 3 && strings.Contains(currentSection, sectionName) {
//inputName := strings.Trim(headingText, " []")
//items = append(items, inputName)
//}
//}
//return ast.GoToNext
//})

//if len(items) == 0 {
//return nil, fmt.Errorf("%s section not found or empty", sectionName)
//}

//return items, nil
//}

//func extractReadmeResources(data string) ([]string, []string, error) {
//extensions := parser.CommonExtensions | parser.AutoHeadingIDs
//p := parser.NewWithExtensions(extensions)
//rootNode := p.Parse([]byte(data))

//var resources []string
//var dataSources []string
//inResourceSection := false

//ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
//if heading, ok := n.(*ast.Heading); ok && entering {
//headingText := extractText(heading)
//if strings.Contains(headingText, "Resources") {
//inResourceSection = true
//} else if heading.Level <= 2 {
//inResourceSection = false
//}
//}

//if inResourceSection && entering {
//if link, ok := n.(*ast.Link); ok {
//linkText := extractText(link)
//if strings.Contains(linkText, "azurerm_") {
//resourceName := strings.Split(linkText, "]")[0]
//resourceName = strings.TrimPrefix(resourceName, "[")
//resources = append(resources, resourceName)

//// Also add the base resource type without the symbolic name
//baseName := strings.Split(resourceName, ".")[0]
//if baseName != resourceName {
//resources = append(resources, baseName)
//}
//}
//}
//}
//return ast.GoToNext
//})

//if len(resources) == 0 && len(dataSources) == 0 {
//return nil, nil, errors.New("resources section not found or empty")
//}

//return resources, dataSources, nil
//}

//func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
//var errors []error
//tfSet := make(map[string]bool)
//mdSet := make(map[string]bool)

//for _, item := range tfItems {
//tfSet[item] = true
//tfSet[strings.Split(item, ".")[0]] = true  // Also add the base resource type
//}
//for _, item := range mdItems {
//mdSet[item] = true
//mdSet[strings.Split(item, ".")[0]] = true  // Also add the base resource type
//}

//for _, tfItem := range tfItems {
//if !mdSet[tfItem] && !mdSet[strings.Split(tfItem, ".")[0]] {
//errors = append(errors, formatError("%s in Terraform but missing in markdown: %s", itemType, tfItem))
//}
//}

//for _, mdItem := range mdItems {
//if !tfSet[mdItem] && !tfSet[strings.Split(mdItem, ".")[0]] {
//errors = append(errors, formatError("%s in markdown but missing in Terraform: %s", itemType, mdItem))
//}
//}

//return errors
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
