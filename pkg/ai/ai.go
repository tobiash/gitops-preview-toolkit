package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/llms/anthropic"
	"github.com/tmc/langchaingo/llms/openai"

	"github.com/tobiash/gitops-preview-toolkit/pkg/config"
	"github.com/tobiash/gitops-preview-toolkit/pkg/diff"
	"github.com/tobiash/gitops-preview-toolkit/pkg/policy"
)

const (
	ProviderOpenAI     = "openai"
	ProviderAnthropic  = "anthropic"
	ProviderZAI        = "zai"
	ProviderMiniMax    = "minimax"
	ProviderOpenRouter = "openrouter"

	ClassificationProvenanceAI = "ai"
)

const (
	defaultMaxInputBytes           = 100_000
	defaultMaxDiffLinesPerResource = 80
	defaultTimeout                 = 30 * time.Second
	defaultOpenAIModel             = "gpt-4o-mini"
	defaultAnthropicModel          = "claude-3-5-haiku-20241022"
	defaultZAIModel                = "glm-5.1"
	defaultMiniMaxModel            = "MiniMax-M2.7"
	defaultOpenRouterModel         = "openai/gpt-5.2"
	zaiBaseURL                     = "https://open.bigmodel.cn/api/paas/v4/"
	minimaxBaseURL                 = "https://api.minimax.io/v1"
	openRouterBaseURL              = "https://openrouter.ai/api/v1"
)

type Assessment struct {
	Provider        string                  `json:"provider"`
	Model           string                  `json:"model"`
	Summary         string                  `json:"summary,omitempty"`
	Classifications []policy.Classification `json:"classifications,omitempty"`
	Truncated       bool                    `json:"truncated,omitempty"`
	Warnings        []string                `json:"warnings,omitempty"`
}

type Request struct {
	Config       *config.AIConfig
	Result       *diff.DiffResult
	PolicyResult *policy.Result
}

type Assessor interface {
	Assess(ctx context.Context, req Request) (*Assessment, error)
}

type LangChainAssessor struct {
	env func(string) string
}

func NewLangChainAssessor() *LangChainAssessor {
	return &LangChainAssessor{env: os.Getenv}
}

func (a *LangChainAssessor) Assess(ctx context.Context, req Request) (*Assessment, error) {
	if req.Config == nil {
		return nil, errors.New("ai assessment config is missing")
	}
	if req.Result == nil || req.Result.TotalChanged() == 0 {
		return nil, nil
	}

	provider, token, err := a.resolveProvider(req.Config)
	if err != nil {
		return nil, err
	}
	model := modelOrDefault(provider, req.Config.Model)

	llm, err := newModel(provider, model, token)
	if err != nil {
		return nil, err
	}

	assessment, err := a.assessWithModel(ctx, llm, provider, model, req, false)
	if err == nil {
		return assessment, nil
	}
	retryAssessment, retryErr := a.assessWithModel(ctx, llm, provider, model, req, true)
	if retryErr != nil {
		return nil, fmt.Errorf("parsing ai assessment response: %w", err)
	}
	return retryAssessment, nil
}

func (a *LangChainAssessor) assessWithModel(ctx context.Context, llm llms.Model, provider, model string, req Request, repair bool) (*Assessment, error) {
	limit := limitsFromConfig(req.Config)
	payload, truncated, err := buildInputPayload(req, limit)
	if err != nil {
		return nil, err
	}

	timeout := timeoutFromConfig(req.Config)
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	system := systemPrompt(req.Config, repair)
	var callOptions []llms.CallOption
	if provider == ProviderMiniMax {
		callOptions = append(callOptions, llms.WithTemperature(0.1))
	} else {
		callOptions = append(callOptions, llms.WithTemperature(0), llms.WithJSONMode())
	}
	response, err := llm.GenerateContent(callCtx, []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, system),
		llms.TextParts(llms.ChatMessageTypeHuman, payload),
	}, callOptions...)
	if err != nil {
		return nil, err
	}
	if len(response.Choices) == 0 {
		return nil, errors.New("empty ai assessment response")
	}

	assessment, err := decodeAssessment(response.Choices[0].Content)
	if err != nil {
		return nil, err
	}
	assessment.Provider = provider
	assessment.Model = model
	assessment.Truncated = assessment.Truncated || truncated
	assessment.Classifications, assessment.Warnings = filterClassifications(assessment.Classifications, req.Config.AllowedClassifications, assessment.Warnings)
	return assessment, nil
}

func (a *LangChainAssessor) resolveProvider(cfg *config.AIConfig) (provider, token string, err error) {
	if cfg.Provider != "" {
		provider = cfg.Provider
		token = a.tokenForProvider(provider)
		if token == "" {
			return "", "", fmt.Errorf("%s token is not set", provider)
		}
		return provider, token, nil
	}

	var found []string
	for _, candidate := range []string{ProviderOpenAI, ProviderAnthropic, ProviderZAI, ProviderMiniMax, ProviderOpenRouter} {
		if a.tokenForProvider(candidate) != "" {
			found = append(found, candidate)
		}
	}
	switch len(found) {
	case 0:
		return "", "", errors.New("no supported ai provider token found")
	case 1:
		provider = found[0]
		return provider, a.tokenForProvider(provider), nil
	default:
		return "", "", errors.New("multiple ai provider tokens found; set ai.provider")
	}
}

func (a *LangChainAssessor) tokenForProvider(provider string) string {
	switch provider {
	case ProviderOpenAI:
		return strings.TrimSpace(a.env("OPENAI_API_KEY"))
	case ProviderAnthropic:
		return strings.TrimSpace(a.env("ANTHROPIC_API_KEY"))
	case ProviderZAI:
		return strings.TrimSpace(a.env("ZAI_API_KEY"))
	case ProviderMiniMax:
		return strings.TrimSpace(a.env("MINIMAX_API_KEY"))
	case ProviderOpenRouter:
		return strings.TrimSpace(a.env("OPENROUTER_API_KEY"))
	default:
		return ""
	}
}

func newModel(provider, model, token string) (llms.Model, error) {
	switch provider {
	case ProviderOpenAI:
		return openai.New(openai.WithToken(token), openai.WithModel(model))
	case ProviderAnthropic:
		return anthropic.New(anthropic.WithToken(token), anthropic.WithModel(model))
	case ProviderZAI:
		return openai.New(openai.WithToken(token), openai.WithModel(model), openai.WithBaseURL(zaiBaseURL))
	case ProviderMiniMax:
		return openai.New(openai.WithToken(token), openai.WithModel(model), openai.WithBaseURL(minimaxBaseURL))
	case ProviderOpenRouter:
		return openai.New(openai.WithToken(token), openai.WithModel(model), openai.WithBaseURL(openRouterBaseURL))
	default:
		return nil, fmt.Errorf("unsupported ai provider %q", provider)
	}
}

func modelOrDefault(provider, model string) string {
	if model != "" {
		return model
	}
	switch provider {
	case ProviderOpenAI:
		return defaultOpenAIModel
	case ProviderAnthropic:
		return defaultAnthropicModel
	case ProviderZAI:
		return defaultZAIModel
	case ProviderMiniMax:
		return defaultMiniMaxModel
	case ProviderOpenRouter:
		return defaultOpenRouterModel
	default:
		return ""
	}
}

type inputLimits struct {
	MaxInputBytes           int
	MaxDiffLinesPerResource int
}

func limitsFromConfig(cfg *config.AIConfig) inputLimits {
	limits := inputLimits{MaxInputBytes: defaultMaxInputBytes, MaxDiffLinesPerResource: defaultMaxDiffLinesPerResource}
	if cfg.MaxInputBytes > 0 {
		limits.MaxInputBytes = cfg.MaxInputBytes
	}
	if cfg.MaxDiffLinesPerResource > 0 {
		limits.MaxDiffLinesPerResource = cfg.MaxDiffLinesPerResource
	}
	return limits
}

func timeoutFromConfig(cfg *config.AIConfig) time.Duration {
	if cfg.Timeout == "" {
		return defaultTimeout
	}
	d, err := time.ParseDuration(cfg.Timeout)
	if err != nil || d <= 0 {
		return defaultTimeout
	}
	return d
}

func systemPrompt(cfg *config.AIConfig, repair bool) string {
	var b strings.Builder
	b.WriteString("You are reviewing rendered Kubernetes manifest changes for Flux GitOps. ")
	b.WriteString("Return only valid JSON matching this schema: ")
	b.WriteString(`{"summary":"markdown summary under 1000 characters","classifications":[{"id":"classification_id","title":"optional title","severity":"info|warning|error","priority":10,"message":"short reason","cluster":"optional","kind":"optional","namespace":"optional","name":"optional","logicalId":"optional"}]}`)
	b.WriteString(". Do not include markdown fences. Classifications may be whole-change scoped or resource scoped. ")
	b.WriteString("For resource-scoped classifications on unnamed resources, copy logicalId from the change; do not invent a name. ")
	if len(cfg.AllowedClassifications) > 0 {
		b.WriteString("Only use these classification ids: ")
		b.WriteString(strings.Join(cfg.AllowedClassifications, ", "))
		b.WriteString(". ")
	}
	if cfg.Instructions != "" {
		b.WriteString("Repository-specific instructions: ")
		b.WriteString(cfg.Instructions)
		b.WriteString(" ")
	}
	if repair {
		b.WriteString("This is a retry because the previous response was invalid JSON or schema-invalid; be stricter. ")
	}
	return b.String()
}

type payload struct {
	Summary   diff.ResultSummary `json:"summary"`
	Policy    *policy.Result     `json:"policy,omitempty"`
	Changes   []payloadChange    `json:"changes"`
	Truncated bool               `json:"truncated,omitempty"`
}

type payloadChange struct {
	Action      string `json:"action"`
	Cluster     string `json:"cluster,omitempty"`
	Kind        string `json:"kind"`
	Namespace   string `json:"namespace,omitempty"`
	Name        string `json:"name"`
	LogicalID   string `json:"logicalId,omitempty"`
	Producer    string `json:"producer,omitempty"`
	UnifiedDiff string `json:"unifiedDiff,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

func buildInputPayload(req Request, limits inputLimits) (string, bool, error) {
	in := payload{Summary: req.Result.Summary(), Policy: req.PolicyResult}
	for _, change := range req.Result.Changes() {
		diffText, truncated := boundedResourceDiff(change, limits.MaxDiffLinesPerResource)
		in.Truncated = in.Truncated || truncated
		in.Changes = append(in.Changes, payloadChange{
			Action:      change.Action,
			Cluster:     change.Cluster,
			Kind:        change.Kind,
			Namespace:   change.Namespace,
			Name:        change.Name,
			LogicalID:   change.LogicalID,
			Producer:    change.Producer,
			UnifiedDiff: diffText,
			Truncated:   truncated,
		})
	}

	data, err := json.Marshal(in)
	if err != nil {
		return "", false, fmt.Errorf("encoding ai assessment input: %w", err)
	}
	if limits.MaxInputBytes > 0 && len(data) > limits.MaxInputBytes {
		data = data[:limits.MaxInputBytes]
		in.Truncated = true
	}
	return string(data), in.Truncated, nil
}

var sensitiveLine = regexp.MustCompile(`(?i)(password|passwd|token|secret|credential|api[_-]?key|private[_-]?key)`)

func boundedResourceDiff(change diff.ResourceChange, maxLines int) (string, bool) {
	if strings.EqualFold(change.Kind, "Secret") {
		return "[redacted secret diff]", false
	}
	lines := strings.Split(change.UnifiedDiff(), "\n")
	truncated := false
	if maxLines > 0 && len(lines) > maxLines {
		lines = lines[:maxLines]
		truncated = true
	}
	for i, line := range lines {
		if sensitiveLine.MatchString(line) {
			lines[i] = redactDiffLine(line)
		}
	}
	return strings.Join(lines, "\n"), truncated
}

func redactDiffLine(line string) string {
	prefix := ""
	if strings.HasPrefix(line, "+") || strings.HasPrefix(line, "-") || strings.HasPrefix(line, " ") {
		prefix = line[:1]
		line = line[1:]
	}
	if idx := strings.Index(line, ":"); idx >= 0 {
		return prefix + line[:idx+1] + " [redacted]"
	}
	return prefix + "[redacted]"
}

func decodeAssessment(raw string) (*Assessment, error) {
	var out Assessment
	data := []byte(strings.TrimSpace(raw))
	if !json.Valid(data) {
		var ok bool
		data, ok = firstJSONObject(data)
		if !ok {
			return nil, fmt.Errorf("response does not contain a JSON object")
		}
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Summary) == "" && len(out.Classifications) == 0 {
		return nil, errors.New("ai assessment response contained no summary or classifications")
	}
	for i := range out.Classifications {
		if out.Classifications[i].ID == "" {
			return nil, errors.New("ai assessment classification id is required")
		}
		out.Classifications[i].Provenance = ClassificationProvenanceAI
	}
	return &out, nil
}

func firstJSONObject(data []byte) ([]byte, bool) {
	start := -1
	depth := 0
	inString := false
	escaped := false
	for i, b := range data {
		if start == -1 {
			if b == '{' {
				start = i
				depth = 1
			}
			continue
		}

		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == '"' {
				inString = false
			}
			continue
		}

		switch b {
		case '"':
			inString = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return data[start : i+1], true
			}
		}
	}
	return nil, false
}

func filterClassifications(items []policy.Classification, allowed []string, warnings []string) ([]policy.Classification, []string) {
	if len(items) == 0 || len(allowed) == 0 {
		return items, warnings
	}
	out := make([]policy.Classification, 0, len(items))
	for _, item := range items {
		if slices.Contains(allowed, item.ID) {
			out = append(out, item)
			continue
		}
		warnings = append(warnings, fmt.Sprintf("dropped disallowed ai classification %q", item.ID))
	}
	return out, warnings
}

func Enabled(cfg *config.AIConfig) bool {
	return cfg != nil && config.BoolOr(cfg.Enabled, false)
}

func FailOnError(cfg *config.AIConfig) bool {
	return cfg != nil && config.BoolOr(cfg.FailOnError, false)
}
