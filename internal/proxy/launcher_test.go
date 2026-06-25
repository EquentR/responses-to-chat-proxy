package proxy

import (
	"encoding/json"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveAndLoadLauncherConfig(t *testing.T) {
	t.Parallel()

	configPath := filepath.Join(t.TempDir(), "config.json")
	config := map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test",
		"model_override":    "",
	}

	if err := saveLauncherConfig(configPath, config); err != nil {
		t.Fatalf("saveLauncherConfig returned error: %v", err)
	}

	content, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("ReadFile returned error: %v", err)
	}

	var parsed map[string]string
	if err := json.Unmarshal(content, &parsed); err != nil {
		t.Fatalf("json.Unmarshal returned error: %v", err)
	}
	assertJSONEqual(t, config, parsed)
	assertJSONEqual(t, config, loadLauncherConfig(configPath))
}

func TestLoadLauncherConfigIgnoresInvalidJSON(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(configPath, []byte("{bad json"), 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	assertJSONEqual(t, map[string]string{}, loadLauncherConfig(configPath))
}

func TestLoadLauncherConfigAcceptsUTF8BOM(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.json")
	content := []byte{0xEF, 0xBB, 0xBF}
	content = append(content, []byte(`{"upstream_base_url":"https://example.com/v1","upstream_api_key":"sk-test","model_override":""}`)...)
	if err := os.WriteFile(configPath, content, 0o600); err != nil {
		t.Fatalf("WriteFile returned error: %v", err)
	}

	assertJSONEqual(t, map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test",
		"model_override":    "",
	}, loadLauncherConfig(configPath))
}

func TestPromptForConfigUsesDefaultBaseURLAndStripsTrailingSlash(t *testing.T) {
	answers := []string{"", " sk-test ", ""}
	index := 0
	prompt := func(string) (string, error) {
		answer := answers[index]
		index++
		return answer, nil
	}

	config, err := promptForConfig(map[string]string{"upstream_base_url": "https://example.com/v1/"}, prompt)
	if err != nil {
		t.Fatalf("promptForConfig returned error: %v", err)
	}

	assertJSONEqual(t, map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test",
		"model_override":    "",
	}, config)
}

func TestApplyRuntimeDefaults(t *testing.T) {
	t.Setenv("UPSTREAM_BASE_URL", "")
	t.Setenv("UPSTREAM_API_KEY", "")
	t.Setenv("PROXY_API_KEY", "")
	t.Setenv("HOST", "")
	t.Setenv("PORT", "")

	applyRuntimeDefaults(map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test",
	}, 8000)

	if os.Getenv("UPSTREAM_BASE_URL") != "https://example.com/v1" {
		t.Fatalf("unexpected UPSTREAM_BASE_URL: %s", os.Getenv("UPSTREAM_BASE_URL"))
	}
	if os.Getenv("UPSTREAM_API_KEY") != "sk-test" {
		t.Fatalf("unexpected UPSTREAM_API_KEY: %s", os.Getenv("UPSTREAM_API_KEY"))
	}
	if os.Getenv("PROXY_API_KEY") != "" || os.Getenv("HOST") != "127.0.0.1" || os.Getenv("PORT") != "8000" {
		t.Fatalf("unexpected runtime defaults")
	}
}

func TestApplyRuntimeDefaultsUsesSelectedPort(t *testing.T) {
	t.Setenv("PORT", "")

	applyRuntimeDefaults(map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test",
	}, 8001)

	if os.Getenv("PORT") != "8001" {
		t.Fatalf("unexpected port: %s", os.Getenv("PORT"))
	}
}

func TestFindAvailablePortSkipsBoundPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen returned error: %v", err)
	}
	defer listener.Close()

	occupiedPort := listener.Addr().(*net.TCPAddr).Port
	selectedPort, err := findAvailablePort(defaultHost, occupiedPort)
	if err != nil {
		t.Fatalf("findAvailablePort returned error: %v", err)
	}
	if selectedPort <= occupiedPort {
		t.Fatalf("expected selected port > occupied port, got %d", selectedPort)
	}
}

func TestShouldReconfigureAcceptsEnterToReuse(t *testing.T) {
	result := shouldReconfigure(map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test-123456",
	}, func(string) (string, error) { return "", nil }, io.Discard)

	if result {
		t.Fatal("expected saved config to be reused")
	}
}

func TestShouldReconfigureReusesConfigWhenInputClosed(t *testing.T) {
	result := shouldReconfigure(map[string]string{
		"upstream_base_url": "https://example.com/v1",
		"upstream_api_key":  "sk-test-123456",
	}, func(string) (string, error) { return "", io.EOF }, io.Discard)

	if result {
		t.Fatal("expected saved config to be reused")
	}
}

func TestMaskAPIKey(t *testing.T) {
	if maskAPIKey("sk-1234567890") != "sk-1...7890" {
		t.Fatalf("unexpected masked key: %s", maskAPIKey("sk-1234567890"))
	}
	if maskAPIKey("short") != "*****" {
		t.Fatalf("unexpected short masked key: %s", maskAPIKey("short"))
	}
}
