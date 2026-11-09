package web

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFile: a file with its folders, for the settings and configs the
// scripts below read.
func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// runEndpointSh runs the host's own words (convoEndpointSh) with a probe
// after them, as a host's shell runs them: the shell is the process the
// probe asks about, so its own environment and command line are what is
// read of it.
func runEndpointSh(t *testing.T, env []string, probe string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh here")
	}
	c := exec.Command("sh", append([]string{"-c", convoEndpointSh + "\n" + probe, "sh"}, args...)...)
	// (A clean environment: the one running the tests has a base URL of its
	// own set now and then, which would answer for the script.)
	c.Env = append([]string{"PATH=" + os.Getenv("PATH")}, env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return string(out)
}

// host_of keeps the host of an address and nothing else of it: not the
// path, not the query, not what signs in — those may carry a key, which is
// not to leave the host.
func TestEndpointHostOf(t *testing.T) {
	for _, c := range []struct{ url, want string }{
		{"https://sub2api.example.com", "sub2api.example.com"},
		{"https://sub2api.example.com/v1", "sub2api.example.com"},
		{"http://relay.example.com:38888/v1?key=0123456789abcdef", "relay.example.com:38888"},
		{"https://someone:secret@relay.example.com/v1", "relay.example.com"},
		{"http://127.0.0.1:24899", "127.0.0.1:24899"},
		{"https://api.anthropic.com", "api.anthropic.com"}, // (named here; dropped in convoEPOf)
		{"not an address", ""},
		{"https://host with a space/v1", ""},
		{"", ""},
	} {
		got := runEndpointSh(t, nil, `host_of "$1"; printf '%s' "$bl"`, c.url)
		if got != c.want {
			t.Errorf("host_of %q: %q, wanted %q", c.url, got, c.want)
		}
	}
}

// setting_env reads a name out of the "env" object of a settings file.
func TestEndpointSettingEnv(t *testing.T) {
	dir := t.TempDir()
	cases := []struct{ name, text, want string }{
		{"plain", `{"env":{"ANTHROPIC_BASE_URL":"https://relay.example.com"}}`, "https://relay.example.com"},
		{"spread", "{\n  \"model\": \"opus\",\n  \"env\": {\n    \"FOO\": \"1\",\n    \"ANTHROPIC_BASE_URL\": \"https://relay.example.com/v1\"\n  }\n}\n", "https://relay.example.com/v1"},
		{"none", `{"model":"opus"}`, ""},
		{"other", `{"env":{"ANTHROPIC_API_KEY":"sk-not-a-real-key"}}`, ""},
	}
	for _, c := range cases {
		p := filepath.Join(dir, c.name+".json")
		writeFile(t, p, c.text)
		got := runEndpointSh(t, nil, `setting_env "$1" ANTHROPIC_BASE_URL; printf '%s' "$pv"`, p)
		if got != c.want {
			t.Errorf("setting_env %s: %q, wanted %q", c.name, got, c.want)
		}
	}
}

// claude_ep: where a Claude Code's requests go and how it signs in, read
// from the environment it started with and from the settings it reads.
func TestEndpointClaude(t *testing.T) {
	probe := `claude_ep $$ "$1"; printf '%s|%s' "$ep" "$auth"`
	home := t.TempDir()  // (no settings of its own: the environment answers)
	empty := t.TempDir() // (nor where it runs)
	for _, c := range []struct {
		name string
		env  []string
		want string
	}{
		{"nothing set: its account", []string{"HOME=" + home}, "|login"},
		{"a token for somewhere else", []string{"HOME=" + home, "ANTHROPIC_BASE_URL=https://relay.example.com/v1", "ANTHROPIC_AUTH_TOKEN=not-a-real-token"}, "relay.example.com|token"},
		{"a key of the maker's", []string{"HOME=" + home, "ANTHROPIC_API_KEY=sk-not-a-real-key"}, "|key"},
		{"Amazon's, which has no address to show", []string{"HOME=" + home, "ANTHROPIC_BASE_URL=https://relay.example.com", "CLAUDE_CODE_USE_BEDROCK=1"}, "|bedrock"},
		{"Google's", []string{"HOME=" + home, "CLAUDE_CODE_USE_VERTEX=true"}, "|vertex"},
		{"the flag off", []string{"HOME=" + home, "CLAUDE_CODE_USE_BEDROCK=0"}, "|login"},
	} {
		if got := runEndpointSh(t, c.env, probe, empty); got != c.want {
			t.Errorf("%s: %q, wanted %q", c.name, got, c.want)
		}
	}

	// The settings win over the environment, as Claude Code itself takes
	// them (both set, the settings' address got the request: seen on
	// 2.1.318), and they are read because the environment is only what it
	// was given when it started.
	withSettings := t.TempDir()
	writeFile(t, filepath.Join(withSettings, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://settings.example.com","ANTHROPIC_AUTH_TOKEN":"not-a-real-token"}}`)
	got := runEndpointSh(t, []string{"HOME=" + withSettings, "ANTHROPIC_BASE_URL=https://environ.example.com"}, probe, empty)
	if got != "settings.example.com|token" {
		t.Errorf("the settings do not win: %q", got)
	}
	// Those of the project it runs in win over the person's own.
	proj := t.TempDir()
	writeFile(t, filepath.Join(proj, ".claude", "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://project.example.com"}}`)
	if got := runEndpointSh(t, []string{"HOME=" + withSettings}, probe, proj); got != "project.example.com|token" {
		t.Errorf("the project's do not win: %q", got)
	}
	// And its own config folder is where it was told to keep it.
	elsewhere := t.TempDir()
	writeFile(t, filepath.Join(elsewhere, "settings.json"), `{"env":{"ANTHROPIC_BASE_URL":"https://told.example.com"}}`)
	if got := runEndpointSh(t, []string{"HOME=" + home, "CLAUDE_CONFIG_DIR=" + elsewhere}, probe, empty); got != "told.example.com|login" {
		t.Errorf("the config folder it was told: %q", got)
	}
}

// codex_ep: the provider its command line or its config names, and that
// provider's address.
func TestEndpointCodex(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), `model_provider = "relay"
model = "gpt-6.1"

[model_providers.relay]
name = "Relay"
base_url = "https://sub2api.example.com/v1"

[model_providers.other]
base_url = "https://other.example.com/v1"

[profiles.second]
model_provider = "other"
`)
	probe := `codex_ep $$; printf '%s' "$ep"`
	if got := runEndpointSh(t, []string{"HOME=" + home}, probe); got != "sub2api.example.com" {
		t.Errorf("the config's provider: %q", got)
	}
	// Its command line wins: the provider named there, or the one the
	// profile named there holds.
	if got := runEndpointSh(t, []string{"HOME=" + home}, probe, "-c", "model_provider=other"); got != "other.example.com" {
		t.Errorf("-c model_provider: %q", got)
	}
	if got := runEndpointSh(t, []string{"HOME=" + home}, probe, "--config=model_provider=other"); got != "other.example.com" {
		t.Errorf("--config=model_provider: %q", got)
	}
	if got := runEndpointSh(t, []string{"HOME=" + home}, probe, "--profile", "second"); got != "other.example.com" {
		t.Errorf("--profile: %q", got)
	}
	if got := runEndpointSh(t, []string{"HOME=" + home}, probe, "--profile=second"); got != "other.example.com" {
		t.Errorf("--profile=: %q", got)
	}
	// A profile its config names, where the command line names none.
	writeFile(t, filepath.Join(home, ".codex", "config.toml"), `profile = "second"

[model_providers.other]
base_url = "https://other.example.com/v1"

[profiles.second]
model_provider = "other"
`)
	if got := runEndpointSh(t, []string{"HOME=" + home}, probe); got != "other.example.com" {
		t.Errorf("the config's profile: %q", got)
	}
	// None named: OpenAI's own, which nothing is shown of, unless the
	// environment sends it elsewhere.
	bare := t.TempDir()
	writeFile(t, filepath.Join(bare, ".codex", "config.toml"), "model = \"gpt-6.1\"\n")
	if got := runEndpointSh(t, []string{"HOME=" + bare}, probe); got != "" {
		t.Errorf("OpenAI's own: %q", got)
	}
	if got := runEndpointSh(t, []string{"HOME=" + bare, "OPENAI_BASE_URL=https://openai-relay.example.com/v1"}, probe); got != "openai-relay.example.com" {
		t.Errorf("OPENAI_BASE_URL: %q", got)
	}
	// Where it was told to keep its config.
	told := t.TempDir()
	writeFile(t, filepath.Join(told, "config.toml"), "model_provider = \"relay\"\n\n[model_providers.relay]\nbase_url = \"https://told.example.com\"\n")
	if got := runEndpointSh(t, []string{"HOME=" + bare, "CODEX_HOME=" + told}, probe); got != "told.example.com" {
		t.Errorf("CODEX_HOME: %q", got)
	}
}

// What the host said of an endpoint is kept only where it is a host's name
// to show, and a sign-in only where it is one of the known ways.
func TestParseConvoLocateEndpoint(t *testing.T) {
	rec := func(f ...string) convoProc {
		t.Helper()
		r := parseConvoLocate(convoRec(append([]string{"pid=100", "start=500", "procstart=500", "sid=" + sidA,
			"log=/home/u/.claude/projects/-w/" + sidA + ".jsonl"}, f...)...)).procs
		if len(r) != 1 {
			t.Fatalf("no proc: %v", f)
		}
		return r[0]
	}
	if p := rec("ep=sub2api.example.com", "auth=token"); p.ep != "sub2api.example.com" || p.auth != "token" {
		t.Errorf("a relay: %+v", p)
	}
	if p := rec("ep=127.0.0.1:24899", "auth=key"); p.ep != "127.0.0.1:24899" || p.auth != "key" {
		t.Errorf("a port of its own: %+v", p)
	}
	// Anthropic's own door is where it goes with nothing set: no endpoint
	// to name, though it was named.
	if p := rec("ep=api.anthropic.com", "auth=login"); p.ep != "" || p.auth != "login" {
		t.Errorf("the maker's own: %+v", p)
	}
	if p := rec("ep=API.Anthropic.COM"); p.ep != "" {
		t.Errorf("the maker's own, said otherwise: %+v", p)
	}
	if p := rec("ep=", "auth="); p.ep != "" || p.auth != "" {
		t.Errorf("nothing said: %+v", p)
	}
	if p := rec("ep=relay.example.com/v1?key=0123456789abcdef"); p.ep != "" {
		t.Errorf("more than a host: %+v", p)
	}
	if p := rec("ep=" + strings.Repeat("a", 121)); p.ep != "" {
		t.Errorf("longer than a host: %+v", p)
	}
	if p := rec("auth=whatever"); p.auth != "" {
		t.Errorf("a way not known: %+v", p)
	}
	// (The record's first value of a key counts, as of every other.)
	if p := rec("ep=first.example.com", "ep=second.example.com"); p.ep != "first.example.com" {
		t.Errorf("the first value: %+v", p)
	}
}
