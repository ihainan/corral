package web

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// Every refusal of a message or a key to an agent (convoPress) has words
// for the person in both languages: an "error.<code>" of the page's, or a
// case of the chat page's own. Without, the gateway's English sentence was
// what the chat page showed ("it waits for an answer, or …", a herdr pane's
// Claude Code with /usage open, 10-31).
func TestConvoPressRefusalsWorded(t *testing.T) {
	src, err := os.ReadFile("conversation.go")
	if err != nil {
		t.Fatal(err)
	}
	body := string(src)
	i := strings.Index(body, "func (s *Server) convoPress(")
	if i < 0 {
		t.Fatal("convoPress not found")
	}
	body = body[i:]
	if j := strings.Index(body[1:], "\nfunc "); j >= 0 {
		body = body[:j+1]
	}
	words, err := os.ReadFile("../../web/src/i18n.ts")
	if err != nil {
		t.Fatal(err)
	}
	chat, err := os.ReadFile("../../web/src/pages/AgentsChat.tsx")
	if err != nil {
		t.Fatal(err)
	}
	// (The Chinese block first, the English after it: each its own.)
	locales := strings.SplitN(string(words), "\n}\n", 3)
	if len(locales) < 2 {
		t.Fatal("i18n.ts: two locales not found")
	}
	codes := map[string]bool{}
	for _, m := range regexp.MustCompile(`writeError\(w, http\.Status\w+, "([a-z_]+)"`).FindAllStringSubmatch(body, -1) {
		codes[m[1]] = true
	}
	if len(codes) < 5 {
		t.Fatalf("only %d codes found in convoPress: the pattern no longer matches", len(codes))
	}
	for code := range codes {
		// (A case of the page's own counts only when it holds for every
		// one: "e.code === 'x')" or "e.code === 'x' ||", not "… && something".)
		if regexp.MustCompile(`e\.code === '` + code + `'(\)| \|\|)`).Match(chat) {
			continue
		}
		for n, l := range locales[:2] {
			if !strings.Contains(l, "'error."+code+"':") {
				t.Errorf("%q: no words for it in locale %d (an 'error.%s' key, or a case in AgentsChat's refusal)", code, n, code)
			}
		}
	}
}
