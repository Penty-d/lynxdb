package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGrammarDataMatchesDocs(t *testing.T) {
	for _, name := range []string{"spl2.ebnf", "examples.jsonl", "llm-cookbook.md"} {
		t.Run(name, func(t *testing.T) {
			docPath := filepath.Join("..", "..", "docs", "grammar", name)
			doc, err := os.ReadFile(docPath)
			if err != nil {
				t.Fatalf("read docs grammar: %v", err)
			}
			bundled, err := grammarFS.ReadFile(filepath.Join("grammar_data", name))
			if err != nil {
				t.Fatalf("read bundled grammar: %v", err)
			}
			if string(bundled) != string(doc) {
				t.Fatalf("bundled grammar_data/%s differs from %s", name, docPath)
			}
		})
	}
}
