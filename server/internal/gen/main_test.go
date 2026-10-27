package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// renderDispatcher renders the request handler template for entries and
// parses it.
func renderDispatcher(t *testing.T, entries []MCPRequestType) *ast.File {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, RenderTemplateToFile(requestHandlerTemplate, dir, "request_handler.go", entries))
	out, err := os.ReadFile(filepath.Join(dir, "request_handler.go"))
	require.NoError(t, err)
	file, err := parser.ParseFile(token.NewFileSet(), "request_handler.go", out, 0)
	require.NoError(t, err)
	return file
}

// isSelector reports whether expr is x.sel.
func isSelector(expr ast.Expr, x, sel string) bool {
	selector, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	ident, ok := selector.X.(*ast.Ident)
	return ok && ident.Name == x && selector.Sel.Name == sel
}

// caseOf is the case of method in a parsed dispatcher.
func caseOf(t *testing.T, file *ast.File, method string) *ast.CaseClause {
	t.Helper()
	var found *ast.CaseClause
	ast.Inspect(file, func(node ast.Node) bool {
		if clause, ok := node.(*ast.CaseClause); ok {
			for _, expr := range clause.List {
				if isSelector(expr, "mcp", method) {
					found = clause
				}
			}
		}
		return found == nil
	})
	require.NotNil(t, found, "no case for %s", method)
	return found
}

// modernBranchOf is the if protocolInfo.Modern statement of clause, nil when
// there is none.
func modernBranchOf(clause *ast.CaseClause) *ast.IfStmt {
	for _, stmt := range clause.Body {
		if branch, ok := stmt.(*ast.IfStmt); ok && isSelector(branch.Cond, "protocolInfo", "Modern") {
			return branch
		}
	}
	return nil
}

// callsOf counts the calls to s.method under node.
func callsOf(node ast.Node, method string) int {
	var calls int
	ast.Inspect(node, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && isSelector(call.Fun, "s", method) {
			calls++
		}
		return true
	})
	return calls
}

// No entry gives tools/call a modern handler today; when one does, its modern
// branch must still run the Final check the legacy branch runs.
func TestAModernHandlerKeepsTheFinalCheck(t *testing.T) {
	entry := MCPRequestType{
		MethodName:       "MethodToolsCall",
		ParamType:        "CallToolRequest",
		ResultType:       "CallToolResult",
		HookName:         "CallTool",
		HandlerFunc:      "handleToolCall",
		ResultIsAny:      true,
		Final:            "guardToolCallResult",
		ModernHandler:    "handleModernToolCall",
		ModernParamType:  "CallToolRequest",
		ModernResultType: "CallToolResult",
		ModernGate:       "servesTools",
	}
	clause := caseOf(t, renderDispatcher(t, []MCPRequestType{entry}), entry.MethodName)
	modern := modernBranchOf(clause)
	require.NotNil(t, modern, "a modern branch")
	assert.Equal(t, 1, callsOf(modern, "handleModernToolCall"), "the modern handler, in the modern branch")
	assert.Equal(t, 1, callsOf(modern, "guardToolCallResult"), "the Final check, in the modern branch")
	assert.Equal(t, 2, callsOf(clause, "guardToolCallResult"), "the Final check, in the modern and the legacy branch")
}
