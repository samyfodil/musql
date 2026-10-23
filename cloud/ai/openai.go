package ai

import (
	"context"
	"errors"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"
)

type openAI struct {
	client openai.Client
	model  string
	max    int64
}

func newOpenAI(cfg Config, baseURL string) *openAI {
	opts := []option.RequestOption{option.WithAPIKey(cfg.APIKey)}
	if baseURL != "" {
		opts = append(opts, option.WithBaseURL(baseURL))
	}
	return &openAI{client: openai.NewClient(opts...), model: cfg.Model, max: int64(cfg.MaxTokens)}
}

func (o *openAI) Generate(ctx context.Context, system, user string) (string, error) {
	res, err := o.client.Chat.Completions.New(ctx, openai.ChatCompletionNewParams{
		Model:       o.model,
		Temperature: openai.Float(0),
		MaxTokens:   openai.Int(o.max),
		Messages: []openai.ChatCompletionMessageParamUnion{
			openai.SystemMessage(system),
			openai.UserMessage(user),
		},
	})
	if err != nil {
		return "", err
	}
	if len(res.Choices) == 0 {
		return "", errors.New("ai: provider returned no choices")
	}
	return res.Choices[0].Message.Content, nil
}
