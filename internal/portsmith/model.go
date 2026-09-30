package portsmith

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"os"
	"strconv"
	"strings"

	agenttypes "github.com/minifish-org/pith/packages/agent/types"
	"github.com/minifish-org/pith/packages/ai/catalog"
)

// ModelConfig is the resolved model connection used by RunPort/RunOptions. The
// API key is intentionally excluded from JSON so it is never serialized.
type ModelConfig struct {
	ID            string
	BaseURL       string
	APIKey        string `json:"-"`
	ContextWindow int
	MaxTokens     int
	Thinking      string
}

// modelCapacity is the known catalog capacity for a model, if available.
type modelCapacity struct {
	ContextWindow int
	MaxTokens     int
}

// lookupEnv mirrors `process.env[name] === undefined`.
type lookupEnv func(string) (string, bool)

func modelLimits(known *modelCapacity, deepseek bool, env lookupEnv) (int, int, error) {
	integer := func(name string, fallback int) (int, error) {
		value, ok := env(name)
		if !ok {
			return fallback, nil
		}
		raw := strings.TrimSpace(value)
		if raw == "" {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		for _, r := range raw {
			if r < '0' || r > '9' {
				return 0, fmt.Errorf("%s must be a positive integer", name)
			}
		}
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || parsed < 1 || parsed > math.MaxInt32 {
			return 0, fmt.Errorf("%s must be a positive integer", name)
		}
		return int(parsed), nil
	}

	defaultContext := 32768
	defaultMax := 4096
	if deepseek {
		defaultMax = 8192
	}
	if known != nil {
		defaultContext = known.ContextWindow
		defaultMax = known.MaxTokens
	}
	contextWindow, err := integer("PORTSMITH_CONTEXT_WINDOW", defaultContext)
	if err != nil {
		return 0, 0, err
	}
	maxTokens, err := integer("PORTSMITH_MAX_TOKENS", defaultMax)
	if err != nil {
		return 0, 0, err
	}

	contextCeiling := 2000000
	if known != nil {
		contextCeiling = known.ContextWindow
	}
	if contextWindow < 8192 || contextWindow > contextCeiling {
		return 0, 0, errors.New("PORTSMITH_CONTEXT_WINDOW must be at least 8192 and within the known model context capacity")
	}
	outputCeiling := 393216
	if known != nil {
		outputCeiling = known.MaxTokens
	}
	if maxTokens < 256 || maxTokens >= contextWindow || maxTokens > outputCeiling {
		return 0, 0, errors.New("PORTSMITH_MAX_TOKENS must be at least 256, below the working context size, and within the known model output capacity")
	}
	return contextWindow, maxTokens, nil
}

// trimDotenvSpace matches the Node parser's `trim_spaces` set.
func trimDotenvSpace(input string) string {
	return strings.Trim(input, " \t\n")
}

// parseDotenv ports Node's `process.loadEnvFile` parser (`src/node_dotenv.cc`).
// It intentionally preserves the Node quirks: carriage returns are removed
// everywhere, only `\n` inside double quotes is expanded, and `#` starts a
// comment for unquoted values.
func parseDotenv(input string) map[string]string {
	store := map[string]string{}
	content := trimDotenvSpace(strings.ReplaceAll(input, "\r", ""))
	for content != "" {
		if content[0] == '\n' || content[0] == '#' {
			if idx := strings.IndexByte(content, '\n'); idx >= 0 {
				content = content[idx+1:]
			} else {
				content = ""
			}
			continue
		}
		equalOrNewline := strings.IndexAny(content, "=\n")
		if equalOrNewline < 0 || content[equalOrNewline] == '\n' {
			if equalOrNewline >= 0 {
				content = trimDotenvSpace(content[equalOrNewline+1:])
				continue
			}
			break
		}
		key := trimDotenvSpace(content[:equalOrNewline])
		content = content[equalOrNewline+1:]
		if content == "" || content[0] == '\n' {
			store[key] = ""
			continue
		}
		content = trimDotenvSpace(content)
		if key == "" {
			continue
		}
		if strings.HasPrefix(key, "export ") {
			key = trimDotenvSpace(key[len("export "):])
		}
		if content == "" {
			store[key] = ""
			break
		}
		if content[0] == '"' {
			if closing := strings.IndexByte(content[1:], '"'); closing >= 0 {
				closing++
				value := strings.ReplaceAll(content[1:closing], `\n`, "\n")
				store[key] = value
				if newline := strings.IndexByte(content[closing+1:], '\n'); newline >= 0 {
					content = content[closing+1+newline+1:]
				} else {
					content = ""
				}
				continue
			}
		}
		if content[0] == '\'' || content[0] == '"' || content[0] == '`' {
			quote := content[0]
			if closing := strings.IndexByte(content[1:], quote); closing >= 0 {
				closing++
				store[key] = content[1:closing]
				if newline := strings.IndexByte(content[closing+1:], '\n'); newline >= 0 {
					content = content[closing+1+newline+1:]
				} else {
					content = ""
				}
				continue
			}
			if newline := strings.IndexByte(content, '\n'); newline >= 0 {
				store[key] = content[:newline]
				content = content[newline+1:]
			} else {
				store[key] = content
				break
			}
		} else {
			if newline := strings.IndexByte(content, '\n'); newline >= 0 {
				value := content[:newline]
				if hash := strings.IndexByte(value, '#'); hash >= 0 {
					value = value[:hash]
				}
				store[key] = trimDotenvSpace(value)
				content = content[newline+1:]
			} else {
				value := content
				if hash := strings.IndexByte(content, '#'); hash >= 0 {
					value = content[:hash]
				}
				store[key] = trimDotenvSpace(value)
				content = ""
			}
		}
		content = trimDotenvSpace(content)
	}
	return store
}

// LoadLocalEnv loads a dotenv file without overriding existing process values.
// An empty file name loads `.env` only when it exists.
func LoadLocalEnv(file string) error {
	if file == "" {
		if _, err := os.Stat(".env"); err != nil {
			return nil
		}
		file = ".env"
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return err
	}
	for key, value := range parseDotenv(string(data)) {
		if _, exists := os.LookupEnv(key); !exists {
			if err := os.Setenv(key, value); err != nil {
				return err
			}
		}
	}
	return nil
}

func firstEnv(names ...string) string {
	for _, name := range names {
		if value, ok := os.LookupEnv(name); ok {
			return value
		}
	}
	return ""
}

// ConfiguredModel resolves the model connection from PORTSMITH_* then OMNI_*
// variables, validates the URL, and fills in Pith's known catalog capacity.
func ConfiguredModel() (ModelConfig, error) {
	baseURL := firstEnv("PORTSMITH_BASE_URL", "OMNI_BASE_URL")
	id := firstEnv("PORTSMITH_MODEL", "OMNI_MODEL")
	key := firstEnv("PORTSMITH_API_KEY", "OMNI_API_KEY")
	if baseURL == "" || id == "" {
		return ModelConfig{}, errors.New("Set PORTSMITH_BASE_URL and PORTSMITH_MODEL, or load existing OMNI_* settings with --env-file")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil {
		return ModelConfig{}, errors.New("Model URL must use HTTP(S) without embedded credentials")
	}
	hasCredentials := false
	if parsed.User != nil {
		_, hasPassword := parsed.User.Password()
		hasCredentials = parsed.User.Username() != "" || hasPassword
	}
	if (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || hasCredentials {
		return ModelConfig{}, errors.New("Model URL must use HTTP(S) without embedded credentials")
	}
	deepseek := strings.EqualFold(parsed.Hostname(), "api.deepseek.com")
	if deepseek && strings.TrimSpace(key) == "" {
		return ModelConfig{}, errors.New("Missing PORTSMITH_API_KEY")
	}
	var known *modelCapacity
	if deepseek {
		if entry, ok := catalog.DEEPSEEK_MODELS[id]; ok {
			known = &modelCapacity{
				ContextWindow: int(entry.Model.ContextWindow),
				MaxTokens:     int(entry.Model.MaxTokens),
			}
		}
	}
	contextWindow, maxTokens, err := modelLimits(known, deepseek, os.LookupEnv)
	if err != nil {
		return ModelConfig{}, err
	}
	return ModelConfig{
		ID:            id,
		BaseURL:       strings.TrimSuffix(baseURL, "/"),
		APIKey:        key,
		ContextWindow: contextWindow,
		MaxTokens:     maxTokens,
		Thinking:      string(agenttypes.ThinkingMedium),
	}, nil
}
