package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

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
	readmePath    string
	data          string
	validators    []Validator
	foundSections map[string]bool
	errors        []error
	mu            sync.RWMutex
}

type SectionValidator struct {
	data     string
	sections []string
	rootNode ast.Node
}

func NewMarkdownValidator(readmePath string) (*MarkdownValidator, error) {
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}
	absReadmePath, err := filepath.Abs(readmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to get absolute path: %v", err)
	}
	data, err := os.ReadFile(absReadmePath)
	if err != nil {
		return nil, fmt.Errorf("failed to read file: %v", err)
	}
	mv := &MarkdownValidator{
		readmePath:    absReadmePath,
		data:          string(data),
		foundSections: make(map[string]bool),
	}

	sectionValidator := NewSectionValidator(mv.data)
	mv.validators = []Validator{
		sectionValidator,
		NewFileValidator(absReadmePath),
		NewURLValidator(mv.data),
		NewTerraformDefinitionValidator(mv.data),
		NewItemValidator(mv.data, "Variables", "variable", []string{"Required Inputs", "Optional Inputs"}, "variables.tf"),
		NewItemValidator(mv.data, "Outputs", "output", []string{"Outputs"}, "outputs.tf"),
	}

	for _, section := range sectionValidator.sections {
		mv.foundSections[section] = sectionValidator.validateSection(section)
	}

	return mv, nil
}

func (mv *MarkdownValidator) Validate() []error {
	var wg sync.WaitGroup
	errChan := make(chan []error, len(mv.validators))

	for _, validator := range mv.validators {
		if itemValidator, ok := validator.(*ItemValidator); ok {
			if !mv.hasSections(itemValidator.sections) {
				continue
			}
		}

		wg.Add(1)
		go func(v Validator) {
			defer wg.Done()
			if errs := v.Validate(); len(errs) > 0 {
				errChan <- errs
			}
		}(validator)
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	var allErrors []error
	errorMap := make(map[string]error) // Deduplicate errors
	for errs := range errChan {
		for _, err := range errs {
			if err != nil {
				errorMap[err.Error()] = err
			}
		}
	}

	for _, err := range errorMap {
		allErrors = append(allErrors, err)
	}
	return allErrors
}

func (mv *MarkdownValidator) hasSections(sections []string) bool {
	mv.mu.RLock()
	defer mv.mu.RUnlock()
	for _, section := range sections {
		if mv.foundSections[section] {
			return true
		}
	}
	return false
}

func (sv *SectionValidator) Validate() []error {
	var allErrors []error
	for _, section := range sv.sections {
		if !sv.validateSection(section) {
			allErrors = append(allErrors, fmt.Errorf("incorrect header: expected '%s', found 'not present'", section))
		}
	}
	return allErrors
}

func NewSectionValidator(data string) *SectionValidator {
	sections := []string{
		"Goals", "Non-Goals", "Resources", "Providers", "Requirements",
		"Optional Inputs", "Required Inputs", "Outputs", "Testing",
	}

	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := markdown.Parse([]byte(data), p)

	return &SectionValidator{data: data, sections: sections, rootNode: rootNode}
}

func (sv *SectionValidator) validateSection(sectionName string) bool {
	found := false
	ast.WalkFunc(sv.rootNode, func(node ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := node.(*ast.Heading); ok && entering && heading.Level == 2 {
			text := strings.TrimSpace(extractText(heading))
			if strings.EqualFold(text, sectionName) ||
				strings.EqualFold(text, sectionName+"s") ||
				(sectionName == "Inputs" && (strings.EqualFold(text, "Required Inputs") || strings.EqualFold(text, "Optional Inputs"))) {
				found = true
				return ast.SkipChildren
			}
		}
		return ast.GoToNext
	})
	return found
}

// FileValidator validates the presence of required files
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
	return &FileValidator{files: files}
}

func (fv *FileValidator) Validate() []error {
	var allErrors []error
	for _, filePath := range fv.files {
		if err := validateFile(filePath); err != nil {
			allErrors = append(allErrors, err)
		}
	}
	return allErrors
}

func validateFile(filePath string) error {
	fileInfo, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("file does not exist: %s", filepath.Base(filePath))
		}
		return fmt.Errorf("error accessing file: %s: %v", filepath.Base(filePath), err)
	}

	if !fileInfo.Mode().IsRegular() {
		return fmt.Errorf("not a regular file: %s", filepath.Base(filePath))
	}

	if fileInfo.Size() == 0 {
		return fmt.Errorf("file is empty: %s", filepath.Base(filePath))
	}

	// Check if file is readable
	file, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("cannot open file: %s: %v", filepath.Base(filePath), err)
	}
	defer file.Close()

	return nil
}

type URLValidator struct {
	data   string
	cache  sync.Map // Cache for URL validation results
	client *http.Client
}

func NewURLValidator(data string) *URLValidator {
	return &URLValidator{
		data: data,
		client: &http.Client{
			Timeout: 10 * time.Second,
			Transport: &http.Transport{
				MaxIdleConns:        100,
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
				DisableKeepAlives:   false,
			},
		},
	}
}

func (uv *URLValidator) Validate() []error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	rxStrict := xurls.Strict()
	urls := rxStrict.FindAllString(uv.data, -1)

	// Deduplicate URLs
	uniqueURLs := make(map[string]struct{})
	for _, u := range urls {
		if !strings.Contains(u, "registry.terraform.io/providers/") {
			uniqueURLs[u] = struct{}{}
		}
	}

	var wg sync.WaitGroup
	errChan := make(chan error, len(uniqueURLs))
	semaphore := make(chan struct{}, 20) // Limit concurrent requests

	for url := range uniqueURLs {
		select {
		case <-ctx.Done():
			return []error{ctx.Err()}
		default:
			wg.Add(1)
			go func(url string) {
				defer wg.Done()
				semaphore <- struct{}{}        // Acquire
				defer func() { <-semaphore }() // Release

				// Check cache first
				if _, ok := uv.cache.Load(url); ok {
					return
				}

				if err := uv.validateSingleURL(ctx, url); err != nil {
					errChan <- err
				} else {
					uv.cache.Store(url, struct{}{})
				}
			}(url)
		}
	}

	go func() {
		wg.Wait()
		close(errChan)
	}()

	var errors []error
	for err := range errChan {
		errors = append(errors, err)
	}

	return errors
}

func (uv *URLValidator) validateSingleURL(ctx context.Context, url string) error {
	req, err := http.NewRequestWithContext(ctx, "HEAD", url, nil)
	if err != nil {
		return fmt.Errorf("error creating request for URL %s: %v", url, err)
	}

	resp, err := uv.client.Do(req)
	if err != nil {
		return fmt.Errorf("error accessing URL: %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("URL returned non-OK status: %s: Status: %d", url, resp.StatusCode)
	}

	return nil
}

// TerraformDefinitionValidator validates Terraform definitions
type TerraformDefinitionValidator struct {
	data string
}

func NewTerraformDefinitionValidator(data string) *TerraformDefinitionValidator {
	return &TerraformDefinitionValidator{data: data}
}

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

type ItemValidator struct {
	data      string
	itemType  string
	blockType string
	sections  []string
	fileName  string
}

func NewItemValidator(data, itemType, blockType string, sections []string, fileName string) *ItemValidator {
	return &ItemValidator{
		data:      data,
		itemType:  itemType,
		blockType: blockType,
		sections:  sections,
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

	var mdItems []string
	for _, section := range iv.sections {
		mdItems = append(mdItems, extractMarkdownSectionItems(iv.data, section)...)
	}

	return compareTerraformAndMarkdown(tfItems, mdItems, iv.itemType)
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

func extractTerraformItems(filePath string, blockType string) ([]string, error) {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
	}

	// Skip empty files
	if len(bytes.TrimSpace(content)) == 0 {
		return nil, nil
	}

	parser := hclparse.NewParser()
	file, parseDiags := parser.ParseHCL(content, filePath)
	if parseDiags.HasErrors() {
		var errMsgs []string
		for _, diag := range parseDiags {
			errMsgs = append(errMsgs, diag.Error())
		}
		return nil, fmt.Errorf("error parsing HCL in %s: %v", filepath.Base(filePath), strings.Join(errMsgs, "; "))
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

func extractMarkdownSectionItems(data string, sectionNames ...string) []string {
	extensions := parser.CommonExtensions | parser.AutoHeadingIDs
	p := parser.NewWithExtensions(extensions)
	rootNode := p.Parse([]byte(data))

	items := make(map[string]struct{}) // Use map for deduplication
	inTargetSection := false

	ast.WalkFunc(rootNode, func(n ast.Node, entering bool) ast.WalkStatus {
		if heading, ok := n.(*ast.Heading); ok && entering {
			headingText := strings.TrimSpace(extractText(heading))
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
				items[inputName] = struct{}{} // Deduplicate entries
			}
		}
		return ast.GoToNext
	})

	// Convert map to slice
	result := make([]string, 0, len(items))
	for item := range items {
		result = append(result, item)
	}
	sort.Strings(result) // Ensure consistent ordering
	return result
}

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

func TestMarkdown(t *testing.T) {
	t.Parallel() // Allow parallel test execution

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	readmePath := "README.md"
	if envPath := os.Getenv("README_PATH"); envPath != "" {
		readmePath = envPath
	}

	done := make(chan struct{})
	var errors []error
	var testErr error

	go func() {
		defer close(done)
		validator, err := NewMarkdownValidator(readmePath)
		if err != nil {
			testErr = err
			return
		}

		errors = validator.Validate()
	}()

	select {
	case <-ctx.Done():
		t.Fatalf("Test timed out after 2 minutes: %v", ctx.Err())
	case <-done:
		if testErr != nil {
			t.Fatalf("Failed to create validator: %v", testErr)
		}
		if len(errors) > 0 {
			for _, err := range errors {
				t.Errorf("Validation error: %v", err)
			}
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
// 	for _, section := range sv.sections {
// 		if !sv.validateSection(section) {
// 			allErrors = append(allErrors, fmt.Errorf("incorrect header: expected '%s', found 'not present'", section))
// 		}
// 	}
// 	return allErrors
// }
//
// func NewSectionValidator(data string) *SectionValidator {
// 	sections := []string{
// 		"Goals", "Non-Goals", "Resources", "Providers", "Requirements",
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
// func extractText(node ast.Node) string {
// 	var sb strings.Builder
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
// 	var resources []string
// 	var dataSources []string
//
// 	workspace := os.Getenv("GITHUB_WORKSPACE")
// 	if workspace == "" {
// 		var err error
// 		workspace, err = os.Getwd()
// 		if err != nil {
// 			return nil, nil, fmt.Errorf("failed to get current working directory: %v", err)
// 		}
// 	}
// 	mainPath := filepath.Join(workspace, "caller", "main.tf")
// 	specificResources, specificDataSources, err := extractFromFilePath(mainPath)
// 	if err != nil && !os.IsNotExist(err) {
// 		return nil, nil, err
// 	}
// 	resources = append(resources, specificResources...)
// 	dataSources = append(dataSources, specificDataSources...)
//
// 	modulesPath := filepath.Join(workspace, "caller", "modules")
// 	modulesResources, modulesDataSources, err := extractRecursively(modulesPath)
// 	if err != nil {
// 		return nil, nil, err
// 	}
// 	resources = append(resources, modulesResources...)
// 	dataSources = append(dataSources, modulesDataSources...)
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
// func extractFromFilePath(filePath string) ([]string, []string, error) {
// 	content, err := os.ReadFile(filePath)
// 	if err != nil {
// 		return nil, nil, fmt.Errorf("error reading file %s: %v", filepath.Base(filePath), err)
// 	}
//
// 	parser := hclparse.NewParser()
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
// 				if strings.Contains(linkText, "azurerm_") {
// 					resourceName := strings.Split(linkText, "]")[0]
// 					resourceName = strings.TrimPrefix(resourceName, "[")
// 					resources = append(resources, resourceName)
//
// 					baseName := strings.Split(resourceName, ".")[0]
// 					if baseName != resourceName {
// 						resources = append(resources, baseName)
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
// func compareTerraformAndMarkdown(tfItems, mdItems []string, itemType string) []error {
// 	var errors []error
// 	tfSet := make(map[string]bool)
// 	mdSet := make(map[string]bool)
// 	reported := make(map[string]bool)
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
//
