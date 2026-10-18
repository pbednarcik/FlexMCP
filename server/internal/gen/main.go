package main

import (
	"bytes"
	_ "embed"
	"fmt"
	"go/format"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"text/template"
)

//go:generate go run . ../..

//go:embed hooks.go.tmpl
var hooksTemplate string

//go:embed request_handler.go.tmpl
var requestHandlerTemplate string

//go:embed dispatch_gates_test.go.tmpl
var dispatchGatesTemplate string

// RenderTemplateToFile renders the template with data and writes it, gofmt'd,
// to fileName in destPath, or to stdout when destPath is "-". Each template
// lists the imports its output uses, so formatting is all it needs.
func RenderTemplateToFile(templateContent, destPath, fileName string, data any) error {
	tmpl, err := template.New(fileName).Funcs(template.FuncMap{
		"toLower": strings.ToLower,
		"title":   func(s string) string { return strings.ToUpper(s[:1]) + s[1:] },
	}).Parse(templateContent)
	if err != nil {
		return err
	}

	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, data); err != nil {
		return err
	}
	processedContent, err := format.Source(rendered.Bytes())
	if err != nil {
		return fmt.Errorf("formatting %s: %w", fileName, err)
	}

	// Write the processed content to the destination
	var destWriter io.Writer
	if destPath == "-" {
		destWriter = os.Stdout
	} else {
		destFile, err := os.Create(filepath.Join(destPath, fileName))
		if err != nil {
			return err
		}
		defer destFile.Close()
		destWriter = destFile
	}

	_, err = destWriter.Write(processedContent)
	return err
}

func main() {
	if len(os.Args) < 2 {
		log.Fatal("usage: gen <destination-directory>")
	}
	destPath := os.Args[1]

	if err := RenderTemplateToFile(hooksTemplate, destPath, "hooks.go", MCPRequestTypes); err != nil {
		log.Fatal(err)
	}

	if err := RenderTemplateToFile(requestHandlerTemplate, destPath, "request_handler.go", MCPRequestTypes); err != nil {
		log.Fatal(err)
	}

	if err := RenderTemplateToFile(dispatchGatesTemplate, destPath, "dispatch_gates_gen_test.go", MCPRequestTypes); err != nil {
		log.Fatal(err)
	}
}
