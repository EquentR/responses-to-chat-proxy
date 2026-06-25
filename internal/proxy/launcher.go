package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
)

const (
	appDirName             = ".responses-to-chat-proxy"
	configFileName         = "config.json"
	defaultUpstreamBaseURL = "https://api.openai.com/v1"
	defaultHost            = "127.0.0.1"
	defaultPort            = 8000
	portSearchLimit        = 100
	defaultLogLevel        = "info"
)

type PromptFunc func(string) (string, error)

func NewConsolePrompter(in io.Reader, out io.Writer) PromptFunc {
	reader := bufio.NewReader(in)
	return func(prompt string) (string, error) {
		if _, err := fmt.Fprint(out, prompt); err != nil {
			return "", err
		}
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		line = strings.TrimSpace(line)
		if errors.Is(err, io.EOF) && line == "" {
			return "", io.EOF
		}
		return line, nil
	}
}

func RunInteractiveMode(out io.Writer, prompt PromptFunc) (Config, error) {
	configPath, err := getConfigPath()
	if err != nil {
		return Config{}, err
	}

	config := loadLauncherConfig(configPath)
	if shouldReconfigure(config, prompt, out) {
		config, err = promptForConfig(config, prompt)
		if err != nil {
			return Config{}, err
		}
		if err := saveLauncherConfig(configPath, config); err != nil {
			return Config{}, err
		}
		fmt.Fprintf(out, "Saved configuration to %s\n", configPath)
	}

	port, err := findAvailablePort(defaultHost, defaultPort)
	if err != nil {
		return Config{}, err
	}

	applyRuntimeDefaults(config, port)
	printStartupMessage(out, port)
	return LoadConfigFromEnv(".env")
}

func getConfigPath() (string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homeDir, appDirName, configFileName), nil
}

func loadLauncherConfig(configPath string) map[string]string {
	content, err := os.ReadFile(configPath)
	if err != nil {
		return map[string]string{}
	}
	content = bytes.TrimPrefix(content, []byte{0xEF, 0xBB, 0xBF})

	var rawConfig map[string]any
	if err := json.Unmarshal(content, &rawConfig); err != nil {
		return map[string]string{}
	}

	config := map[string]string{}
	for _, key := range []string{"upstream_base_url", "upstream_api_key", "model_override"} {
		if value := stringValue(rawConfig[key]); value != "" || key == "model_override" {
			config[key] = value
		}
	}
	return config
}

func shouldReconfigure(config map[string]string, prompt PromptFunc, out io.Writer) bool {
	if !isCompleteConfig(config) {
		return true
	}

	fmt.Fprintln(out, "Loaded saved upstream configuration:")
	fmt.Fprintf(out, "  base_url: %s\n", config["upstream_base_url"])
	fmt.Fprintf(out, "  api key : %s\n", maskAPIKey(config["upstream_api_key"]))
	answer, err := prompt("Press Enter to start, or type r to reconfigure: ")
	if err != nil {
		return false
	}
	answer = strings.ToLower(strings.TrimSpace(answer))
	return answer == "r" || answer == "reconfigure" || answer == "y" || answer == "yes"
}

func isCompleteConfig(config map[string]string) bool {
	return strings.TrimSpace(config["upstream_base_url"]) != "" && strings.TrimSpace(config["upstream_api_key"]) != ""
}

func promptForConfig(existingConfig map[string]string, prompt PromptFunc) (map[string]string, error) {
	defaultBaseURL := existingConfig["upstream_base_url"]
	if defaultBaseURL == "" {
		defaultBaseURL = defaultUpstreamBaseURL
	}

	upstreamBaseURL, err := promptRequired(prompt, fmt.Sprintf("Upstream base_url [%s]: ", defaultBaseURL), defaultBaseURL)
	if err != nil {
		return nil, err
	}

	upstreamAPIKey, err := promptSecretRequired(prompt, "Upstream api key: ")
	if err != nil {
		return nil, err
	}

	defaultModel := existingConfig["model_override"]
	modelOverride, err := prompt(fmt.Sprintf("Model override (leave empty to use client's model) [%s]: ", defaultModel))
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	modelOverride = strings.TrimSpace(modelOverride)
	if modelOverride == "" && defaultModel != "" {
		modelOverride = defaultModel
	}

	return map[string]string{
		"upstream_base_url": strings.TrimRight(upstreamBaseURL, "/"),
		"upstream_api_key":  strings.TrimSpace(upstreamAPIKey),
		"model_override":    modelOverride,
	}, nil
}

func promptRequired(promptFn PromptFunc, promptText, fallback string) (string, error) {
	for {
		value, err := promptFn(promptText)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return value, nil
		}
		if fallback != "" {
			return fallback, nil
		}
	}
}

func promptSecretRequired(promptFn PromptFunc, promptText string) (string, error) {
	for {
		value, err := promptFn(promptText)
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		value = strings.TrimSpace(value)
		if value != "" {
			return value, nil
		}
	}
}

func saveLauncherConfig(configPath string, config map[string]string) error {
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		return err
	}

	content, err := json.MarshalIndent(map[string]string{
		"upstream_base_url": config["upstream_base_url"],
		"upstream_api_key":  config["upstream_api_key"],
		"model_override":    config["model_override"],
	}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(configPath, append(content, '\n'), 0o600)
}

func applyRuntimeDefaults(config map[string]string, port int) {
	_ = os.Setenv("UPSTREAM_BASE_URL", config["upstream_base_url"])
	_ = os.Setenv("UPSTREAM_API_KEY", config["upstream_api_key"])
	_ = os.Setenv("PROXY_API_KEY", "")
	_ = os.Setenv("MODEL_OVERRIDE", config["model_override"])
	_ = os.Setenv("HOST", defaultHost)
	_ = os.Setenv("PORT", fmt.Sprintf("%d", port))
	if _, exists := os.LookupEnv("LOG_LEVEL"); !exists {
		_ = os.Setenv("LOG_LEVEL", defaultLogLevel)
	}
}

func printStartupMessage(out io.Writer, port int) {
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Responses Chat Proxy is starting.")
	fmt.Fprintf(out, "Local Responses API base_url: http://%s:%d/v1\n", defaultHost, port)
	if modelOverride := os.Getenv("MODEL_OVERRIDE"); modelOverride != "" {
		fmt.Fprintf(out, "Model override: %s\n", modelOverride)
	}
	fmt.Fprintf(out, "Responses endpoint: http://%s:%d/v1/responses\n", defaultHost, port)
	fmt.Fprintln(out, "Proxy authentication is disabled for local clients.")
	fmt.Fprintln(out, "Press Ctrl+C to stop the service.")
	fmt.Fprintln(out)
}

func findAvailablePort(host string, startPort int) (int, error) {
	for port := startPort; port < startPort+portSearchLimit; port++ {
		address := fmt.Sprintf("%s:%d", host, port)
		conn, err := net.DialTimeout("tcp", address, 200000000)
		if err == nil {
			_ = conn.Close()
			continue
		}

		listener, err := net.Listen("tcp", address)
		if err != nil {
			continue
		}
		_ = listener.Close()
		return port, nil
	}
	return 0, fmt.Errorf("no available local port found from %d to %d", startPort, startPort+portSearchLimit-1)
}

func maskAPIKey(apiKey string) string {
	if len(apiKey) <= 8 {
		return strings.Repeat("*", len(apiKey))
	}
	return apiKey[:4] + "..." + apiKey[len(apiKey)-4:]
}
