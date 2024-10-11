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
        NewItemValidator(data, "Variables", "variable", "variables.tf", []string{"Required Inputs", "Optional Inputs"}),
        NewItemValidator(data, "Outputs", "output", "outputs.tf", []string{"Outputs"}),
    }

    return mv, nil
}

// Validate runs all registered validators
//func (mv *MarkdownValidator) Validate() []error {
    //var allErrors []error
    //for _, validator := range mv.validators {
        //allErrors = append(allErrors, validator.Validate()...)
    //}
    //return allErrors
//}

func (mv *MarkdownValidator) Validate() []error {
    var allErrors []error
    for _, validator := range mv.validators {
        errors := validator.Validate()
        if len(errors) > 0 {
            fmt.Printf("Validator %T found errors: %v\n", validator, errors)
        }
        allErrors = append(allErrors, errors...)
    }

    if len(allErrors) == 0 {
        fmt.Println("No validation errors found")
    } else {
        fmt.Printf("Total validation errors: %d\n", len(allErrors))
    }

    return allErrors
}

// Section represents a markdown section
type Section struct {
    Headers []string
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
        {Headers: []string{"Goals"}},
        {Headers: []string{"Resources"}},
        {Headers: []string{"Providers"}},
        {Headers: []string{"Requirements"}},
        {Headers: []string{"Required Inputs", "Optional Inputs"}},
        {Headers: []string{"Outputs"}},
        //{Headers: []string{"Features"}},
        {Headers: []string{"Testing"}},
        //{Headers: []string{"Authors"}},
        //{Headers: []string{"License"}},
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
//func (sv *SectionValidator) Validate() []error {
    //var allErrors []error
    //for _, section := range sv.sections {
        //allErrors = append(allErrors, section.validate(sv.rootNode)...)
    //}
    //return allErrors
//}

func (sv *SectionValidator) Validate() []error {
    var allErrors []error
    for _, section := range sv.sections {
        errors := section.validate(sv.rootNode)
        if len(errors) > 0 {
            fmt.Printf("SectionValidator found errors for section: %v\n", section.Headers)
        }
        allErrors = append(allErrors, errors...)
    }

    if len(allErrors) == 0 {
        fmt.Println("SectionValidator found no errors")
    }

    return allErrors
}

// validate checks if a section is present in the markdown
func (s Section) validate(rootNode ast.Node) []error {
    var errors []error
    found := false

    // Traverse the AST to find the section header
    ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
        if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
            text := strings.TrimSpace(extractText(heading))
            for _, header := range s.Headers {
                if strings.EqualFold(text, header) {
                    found = true
                    return ast.SkipChildren
                }
            }
        }
        return ast.GoToNext
    })

    if !found {
        errors = append(errors, compareHeaders(strings.Join(s.Headers, " or "), ""))
    }

    return errors
}

// FileValidator validates the presence of required files
type FileValidator struct {
    files []string
}

func NewFileValidator(readmePath string) *FileValidator {
    rootDir := filepath.Dir(readmePath)
    files := []string{
        readmePath,
        //filepath.Join(rootDir, "CONTRIBUTING.md"),
        //filepath.Join(rootDir, "CODE_OF_CONDUCT.md"),
        //filepath.Join(rootDir, "SECURITY.md"),
        //filepath.Join(rootDir, "LICENSE"),
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
    // Extract resources from Terraform files
    tfResources, tfDataSources, err := extractTerraformResources()
    if err != nil {
        return []error{err}
    }

    // Extract resources from Markdown
    readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
    if err != nil {
        return []error{err}
    }

    fmt.Printf("Terraform Resources: %v\n", tfResources)
    fmt.Printf("Markdown Resources: %v\n", readmeResources)

    var errors []error

    // If both Terraform and Markdown resources are empty, raise the error
    if len(tfResources) == 0 && len(readmeResources) == 0 {
        fmt.Println("Both Terraform and Markdown resources are empty")
        errors = append(errors, fmt.Errorf("resources section not found or empty"))
    } else {
        // Compare resources only if either Terraform or Markdown resources exist
        if len(tfResources) > 0 || len(readmeResources) > 0 {
            fmt.Println("Comparing Terraform and Markdown resources")
            errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
        }

        // Compare data sources if applicable
        if len(tfDataSources) > 0 || len(readmeDataSources) > 0 {
            errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)
        }
    }

    if len(errors) == 0 {
        fmt.Println("No validation errors in TerraformDefinitionValidator.")
    } else {
        fmt.Printf("Errors found in TerraformDefinitionValidator: %v\n", errors)
    }

    return errors
}


// Validate compares Terraform resources with those documented in the markdown
//func (tdv *TerraformDefinitionValidator) Validate() []error {
    //// Extract resources from Terraform files
    //tfResources, tfDataSources, err := extractTerraformResources()
    //if err != nil {
        //return []error{err}
    //}

    //// Extract resources from Markdown
    //readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
    //if err != nil {
        //return []error{err}
    //}

    //// Initialize errors
    //var errors []error

    //// If BOTH Terraform and Markdown have no resources, raise an error
    //if len(tfResources) == 0 && len(readmeResources) == 0 {
        //return []error{fmt.Errorf("resources section not found or empty")}
    //}

    //// Compare resources if present
    //if len(tfResources) > 0 || len(readmeResources) > 0 {
        //errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
        //errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)
    //}

    //if len(errors) == 0 {
        //fmt.Println("No validation errors in TerraformDefinitionValidator.")
    //} else {
        //fmt.Printf("Errors found in TerraformDefinitionValidator: %v\n", errors)
    //}

    //return errors
//}

// Validate compares Terraform resources with those documented in the markdown
//func (tdv *TerraformDefinitionValidator) Validate() []error {
    //// Extract resources from Terraform files
    //tfResources, tfDataSources, err := extractTerraformResources()
    //if err != nil {
        //return []error{err}
    //}

    //// Extract resources from Markdown
    //readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
    //if err != nil {
        //return []error{err}
    //}

    //// Initialize errors
    //var errors []error

    //// Only raise "resources section not found" error if BOTH Terraform and Markdown resources are empty
    //if len(tfResources) == 0 && len(readmeResources) == 0 {
        //errors = append(errors, fmt.Errorf("resources section not found or empty"))
    //} else {
        //// Perform comparison if resources are present
        //resourcesErrors := compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")
        //if len(resourcesErrors) == 0 {
            //fmt.Println("Terraform and Markdown resources match, no errors.")
        //} else {
            //errors = append(errors, resourcesErrors...)
        //}

        //dataSourcesErrors := compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")
        //errors = append(errors, dataSourcesErrors...)
    //}

    //if len(errors) == 0 {
        //fmt.Println("No validation errors in TerraformDefinitionValidator.")
    //} else {
        //fmt.Printf("Errors found in TerraformDefinitionValidator: %v\n", errors)
    //}

    //return errors
//}

// Validate compares Terraform resources with those documented in the markdown
// Validate compares Terraform resources with those documented in the markdown
//func (tdv *TerraformDefinitionValidator) Validate() []error {
    //// Extract resources from Terraform files
    //tfResources, tfDataSources, err := extractTerraformResources()
    //if err != nil {
        //return []error{err}
    //}

    //fmt.Printf("Terraform Resources: %v\n", tfResources)

    //// Extract resources from Markdown
    //readmeResources, readmeDataSources, err := extractReadmeResources(tdv.data)
    //if err != nil {
        //return []error{err}
    //}

    //fmt.Printf("Markdown Resources: %v\n", readmeResources)

    //// Compare the two sets of resources
    //var errors []error
    //errors = append(errors, compareTerraformAndMarkdown(tfResources, readmeResources, "Resources")...)
    //errors = append(errors, compareTerraformAndMarkdown(tfDataSources, readmeDataSources, "Data Sources")...)

    //if len(errors) == 0 {
        //fmt.Println("No errors in TerraformDefinitionValidator")
    //} else {
        //fmt.Printf("Errors in TerraformDefinitionValidator: %v\n", errors)
    //}

    //return errors
//}

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

// ItemValidator validates items in Terraform and markdown
type ItemValidator struct {
    data      string
    itemType  string
    blockType string
    fileName  string
    sections  []string
}

// NewItemValidator creates a new ItemValidator
func NewItemValidator(data, itemType, blockType, fileName string, sections []string) *ItemValidator {
    return &ItemValidator{
        data:      data,
        itemType:  itemType,
        blockType: blockType,
        fileName:  fileName,
        sections:  sections,
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
    for _, section := range iv.sections {
        sectionItems, err := extractMarkdownSectionItems(iv.data, section)
        if err != nil {
            continue // Ignore the error if section not found
        }
        mdItems = append(mdItems, sectionItems...)
    }

    if len(mdItems) == 0 {
        return []error{fmt.Errorf("%s sections not found or empty", strings.Join(iv.sections, ", "))}
    }

    return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
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

// equalSlices checks if two slices are equal regardless of order
func equalSlices(a, b []string) bool {
    if len(a) != len(b) {
        return false
    }
    aSet := make(map[string]struct{}, len(a))
    for _, v := range a {
        aSet[v] = struct{}{}
    }
    for _, v := range b {
        if _, exists := aSet[v]; !exists {
            return false
        }
    }
    return true
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

// compareTerraformAndMarkdown compares items in Terraform and markdown
func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
    var errors []error

    fmt.Printf("Comparing %s between Terraform and Markdown\n", itemType)
    fmt.Printf("Terraform %s: %v\n", itemType, tfItems)
    fmt.Printf("Markdown %s: %v\n", itemType, mdItems)

    // Check for missing resources in Markdown
    missingInMarkdown := findMissingItems(tfItems, mdItems)
    if len(missingInMarkdown) > 0 {
        fmt.Printf("Missing in markdown: %v\n", missingInMarkdown)
        errors = append(errors, formatError("%s missing in markdown:\n  %s", itemType, strings.Join(missingInMarkdown, "\n  ")))
    } else {
        fmt.Println("No resources missing in Markdown")
    }

    // Check for missing resources in Terraform
    missingInTerraform := findMissingItems(mdItems, tfItems)
    if len(missingInTerraform) > 0 {
        fmt.Printf("Missing in Terraform: %v\n", missingInTerraform)
        errors = append(errors, formatError("%s in markdown but missing in Terraform:\n  %s", itemType, strings.Join(missingInTerraform, "\n  ")))
    } else {
        fmt.Println("No resources missing in Terraform")
    }

    return errors
}

// compareTerraformAndMarkdown compares items in Terraform and markdown
//func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
    //var errors []error

    //fmt.Printf("Comparing %s in Terraform and Markdown\n", itemType)
    //fmt.Printf("Terraform %s: %v\n", itemType, tfItems)
    //fmt.Printf("Markdown %s: %v\n", itemType, mdItems)

    //missingInMarkdown := findMissingItems(tfItems, mdItems)
    //if len(missingInMarkdown) > 0 {
        //fmt.Printf("Missing in markdown: %v\n", missingInMarkdown)
        //errors = append(errors, formatError("%s missing in markdown:\n  %s", itemType, strings.Join(missingInMarkdown, "\n  ")))
    //}

    //missingInTerraform := findMissingItems(mdItems, tfItems)
    //if len(missingInTerraform) > 0 {
        //fmt.Printf("Missing in Terraform: %v\n", missingInTerraform)
        //errors = append(errors, formatError("%s in markdown but missing in Terraform:\n  %s", itemType, strings.Join(missingInTerraform, "\n  ")))
    //}

    //return errors
//}
//func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
    //var errors []error

    //fmt.Printf("Terraform %s: %v\n", itemType, tfItems)
    //fmt.Printf("Markdown %s: %v\n", itemType, mdItems)

    //missingInMarkdown := findMissingItems(tfItems, mdItems)
    //if len(missingInMarkdown) > 0 {
        //fmt.Printf("Missing in markdown: %v\n", missingInMarkdown)
        //errors = append(errors, formatError("%s missing in markdown:\n  %s", itemType, strings.Join(missingInMarkdown, "\n  ")))
    //}

    //missingInTerraform := findMissingItems(mdItems, tfItems)
    //if len(missingInTerraform) > 0 {
        //fmt.Printf("Missing in Terraform: %v\n", missingInTerraform)
        //errors = append(errors, formatError("%s in markdown but missing in Terraform:\n  %s", itemType, strings.Join(missingInTerraform, "\n  ")))
    //}

    //return errors
//}

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
    hclContent, _, diags := body.PartialContent(&hcl.BodySchema{
        Blocks: []hcl.BlockHeaderSchema{
            {Type: blockType, LabelNames: []string{"name"}},
        },
    })

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
            if strings.EqualFold(text, sectionName) {
                inTargetSection = true
                return ast.GoToNext
            }
            inTargetSection = false
        }

        if inTargetSection {
            if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 3 {
                // Extract the variable or output name from the heading
                text := strings.TrimSpace(extractText(heading))
                // The text may look like: "<a name="input_name"></a> [name](#input_name)"
                // Extract the name inside the square brackets
                nameStart := strings.Index(text, "[")
                nameEnd := strings.Index(text, "]")
                if nameStart >= 0 && nameEnd > nameStart {
                    itemName := text[nameStart+1 : nameEnd]
                    items = append(items, itemName)
                } else {
                    // If no square brackets, take the whole text
                    items = append(items, text)
                }
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
        // Print each heading we encounter for debugging
        if heading, ok := node.(*ast.Heading); ok && entering {
            text := strings.TrimSpace(extractText(heading))
            fmt.Printf("Heading found: Level=%d, Text=%s\n", heading.Level, text)
            if heading.Level == 2 && strings.EqualFold(text, "Resources") {
                fmt.Println("Entering Resources section")
                inResourcesSection = true
            } else if heading.Level == 2 {
                inResourcesSection = false
            }
        }

        // Check if we're in the "Resources" section and print list items found
        if inResourcesSection && entering {
            if listItem, ok := node.(*ast.ListItem); ok {
                resourceText := extractText(listItem)
                fmt.Printf("Resource item found: %s\n", resourceText)
                // Extract resource name and type
                nameStart := strings.Index(resourceText, "[")
                nameEnd := strings.Index(resourceText, "]")
                if nameStart >= 0 && nameEnd > nameStart {
                    resourceName := resourceText[nameStart+1 : nameEnd]
                    if strings.Contains(resourceText, "(resource)") {
                        resources = append(resources, resourceName)
                    } else if strings.Contains(resourceText, "(data source)") {
                        dataSources = append(dataSources, resourceName)
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


// extractReadmeResources extracts resources and data sources from the markdown
//func extractReadmeResources(data string) ([]string, []string, error) {
    //extensions := parser.CommonExtensions | parser.AutoHeadingIDs
    //p := parser.NewWithExtensions(extensions)
    //rootNode := markdown.Parse([]byte(data), p)

    //var resources []string
    //var dataSources []string
    //var inResourcesSection bool

    //ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
        //if heading, ok := node.(*ast.Heading); ok && entering {
            //if heading.Level <= 2 {
                //text := strings.TrimSpace(extractText(heading))
                //if strings.EqualFold(text, "Resources") {
                    //inResourcesSection = true
                //} else {
                    //if inResourcesSection {
                        //// We have exited the Resources section
                        //inResourcesSection = false
                    //}
                //}
            //}
        //}

        //if inResourcesSection && entering {
            //if listItem, ok := node.(*ast.ListItem); ok {
                //resourceText := extractText(listItem)
                //// Extract resource name and type
                //nameStart := strings.Index(resourceText, "[")
                //nameEnd := strings.Index(resourceText, "]")
                //if nameStart >= 0 && nameEnd > nameStart {
                    //resourceName := resourceText[nameStart+1 : nameEnd]
                    //// Extract resource type
                    //typeStart := strings.LastIndex(resourceText, "(")
                    //typeEnd := strings.LastIndex(resourceText, ")")
                    //if typeStart >= 0 && typeEnd > typeStart {
                        //resourceType := resourceText[typeStart+1 : typeEnd]
                        //resourceType = strings.TrimSpace(resourceType)
                        //if strings.EqualFold(resourceType, "resource") {
                            //resources = append(resources, resourceName)
                        //} else if strings.EqualFold(resourceType, "data source") {
                            //dataSources = append(dataSources, resourceName)
                        //}
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

//func extractReadmeResources(data string) ([]string, []string, error) {
    //extensions := parser.CommonExtensions | parser.AutoHeadingIDs
    //p := parser.NewWithExtensions(extensions)
    //rootNode := markdown.Parse([]byte(data), p)

    //var resources []string
    //var dataSources []string
    //var inResourcesSection bool

    //ast.WalkFunc(rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
        //if heading, ok := node.(*ast.Heading); ok && entering {
            //if heading.Level == 2 {
                //text := strings.TrimSpace(extractText(heading))
                //if strings.EqualFold(text, "Resources") {
                    //inResourcesSection = true
                //} else {
                    //if inResourcesSection {
                        //// We have moved past the Resources section
                        //inResourcesSection = false
                    //}
                //}
            //}
        //}

        //if inResourcesSection && entering {
            //if listItem, ok := node.(*ast.ListItem); ok {
                //resourceText := extractText(listItem)
                //// Extract resource name and type
                //nameStart := strings.Index(resourceText, "[")
                //nameEnd := strings.Index(resourceText, "]")
                //if nameStart >= 0 && nameEnd > nameStart {
                    //resourceName := resourceText[nameStart+1 : nameEnd]
                    //// Extract resource type
                    //typeStart := strings.LastIndex(resourceText, "(")
                    //typeEnd := strings.LastIndex(resourceText, ")")
                    //if typeStart >= 0 && typeEnd > typeStart {
                        //resourceType := resourceText[typeStart+1 : typeEnd]
                        //resourceType = strings.TrimSpace(resourceType)
                        //if strings.EqualFold(resourceType, "resource") {
                            //resources = append(resources, resourceName)
                        //} else if strings.EqualFold(resourceType, "data source") {
                            //dataSources = append(dataSources, resourceName)
                        //}
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

    callerPath := filepath.Join(workspace, "caller")
    fmt.Printf("Extracting Terraform resources from path: %s\n", callerPath)

    allResources, allDataSources, err := extractRecursively(callerPath)
    if err != nil {
        return nil, nil, err
    }

    fmt.Printf("Terraform resources extracted: %v\n", allResources)
    fmt.Printf("Terraform data sources extracted: %v\n", allDataSources)

    resources = append(resources, allResources...)
    dataSources = append(dataSources, allDataSources...)

    return resources, dataSources, nil
}

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

    //callerPath := filepath.Join(workspace, "caller")
    //allResources, allDataSources, err := extractRecursively(callerPath)
    //if err != nil {
        //return nil, nil, err
    //}

    //resources = append(resources, allResources...)
    //dataSources = append(dataSources, allDataSources...)

    //return resources, dataSources, nil
//}

// extractRecursively extracts resources and data sources recursively, skipping specified directories
func extractRecursively(dirPath string) ([]string, []string, error) {
    var resources []string
    var dataSources []string
    if _, err := os.Stat(dirPath); os.IsNotExist(err) {
        return resources, dataSources, nil
    } else if err != nil {
        return nil, nil, err
    }

    // Directories to skip
    skipDirs := map[string]struct{}{
        "modules":  {},
        "examples": {},
    }

    err := filepath.Walk(dirPath, func(path string, info os.FileInfo, err error) error {
        if err != nil {
            return err
        }

        // Skip the modules and examples directories
        if info.IsDir() {
            if _, shouldSkip := skipDirs[info.Name()]; shouldSkip {
                return filepath.SkipDir
            }
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
    fmt.Printf("Extracting from Terraform file: %s\n", filePath)

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
        // No relevant blocks found
        return resources, dataSources, nil
    }

    for _, block := range hclContent.Blocks {
        if len(block.Labels) >= 2 {
            resourceType := strings.TrimSpace(block.Labels[0])
            resourceName := strings.TrimSpace(block.Labels[1])
            fullResourceName := resourceType + "." + resourceName
            if block.Type == "resource" {
                resources = append(resources, fullResourceName)
                fmt.Printf("Resource found: %s\n", fullResourceName)
            } else if block.Type == "data" {
                dataSources = append(dataSources, fullResourceName)
                fmt.Printf("Data source found: %s\n", fullResourceName)
            }
        }
    }

    return resources, dataSources, nil
}
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

    //// Use PartialContent to allow unknown blocks
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
        //// No relevant blocks found
        //return resources, dataSources, nil
    //}

    //for _, block := range hclContent.Blocks {
        //if len(block.Labels) >= 2 {
            //resourceType := strings.TrimSpace(block.Labels[0])
            //resourceName := strings.TrimSpace(block.Labels[1])
            //fullResourceName := resourceType + "." + resourceName
            //if block.Type == "resource" {
                //resources = append(resources, fullResourceName)
            //} else if block.Type == "data" {
                //dataSources = append(dataSources, fullResourceName)
            //}
        //}
    //}

    //return resources, dataSources, nil
//}

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
