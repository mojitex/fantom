package optimizer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type HeadroomStats struct {
	OriginalBytes   int
	CompressedBytes int
	MessagesChanged int
	TokensSaved     int64
	TokensBefore    int64
	TokensAfter     int64
}

type headroomMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
}

type headroomTarget struct {
	index   int
	content string
}

type headroomResponse struct {
	Messages []struct {
		Role       string `json:"role"`
		Content    string `json:"content"`
		ToolCallID string `json:"tool_call_id"`
	} `json:"messages"`
	TokensBefore int64 `json:"tokens_before"`
	TokensAfter  int64 `json:"tokens_after"`
	TokensSaved  int64 `json:"tokens_saved"`
}

// CompressWithHeadroom projects only plain-text message content to Headroom.
// Original message objects (tool calls, JSON metadata, media blocks and extra
// provider fields) remain untouched. Any network, validation or response error
// fails open and returns the original messages.
func CompressWithHeadroom(ctx context.Context, baseURL, model string, rawMessages json.RawMessage, timeout time.Duration, compressUserMessages bool) (json.RawMessage, HeadroomStats, error) {
	var stats HeadroomStats
	if timeout <= 0 { timeout = 3 * time.Second }
	if timeout > 30*time.Second { timeout = 30 * time.Second }
	endpoint, err := headroomEndpoint(baseURL)
	if err != nil { return rawMessages, stats, err }

	var original []json.RawMessage
	if err := json.Unmarshal(rawMessages, &original); err != nil {
		return rawMessages, stats, fmt.Errorf("decode messages: %w", err)
	}
	if len(original) == 0 { return rawMessages, stats, errors.New("request has no messages") }

	projected := make([]headroomMessage, len(original))
	targets := make([]headroomTarget, 0, len(original))
	for i, raw := range original {
		var message map[string]json.RawMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return rawMessages, stats, fmt.Errorf("decode message %d: %w", i, err)
		}
		_ = json.Unmarshal(message["role"], &projected[i].Role)
		_ = json.Unmarshal(message["tool_call_id"], &projected[i].ToolCallID)
		_ = json.Unmarshal(message["name"], &projected[i].Name)
		projected[i].ToolCalls = message["tool_calls"]
		if len(message["content"]) > 0 && string(message["content"]) != "null" {
			var text string
			if json.Unmarshal(message["content"], &text) == nil && strings.TrimSpace(text) != "" {
				projected[i].Content = text
				targets = append(targets, headroomTarget{index:i,content:text})
			}
		}
	}
	if len(targets) == 0 { return rawMessages, stats, errors.New("no plain-text content eligible for compression") }

	requestBody := map[string]any{"messages":projected,"model":model}
	if compressUserMessages {
		requestBody["config"] = map[string]bool{"compress_user_messages":true}
	}
	encoded, err := json.Marshal(requestBody)
	if err != nil { return rawMessages, stats, fmt.Errorf("encode Headroom request: %w",err) }

	requestCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx,http.MethodPost,endpoint,bytes.NewReader(encoded))
	if err != nil { return rawMessages, stats, err }
	req.Header.Set("Content-Type","application/json")
	req.Header.Set("Accept","application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil { return rawMessages, stats, fmt.Errorf("Headroom request failed: %w",err) }
	defer resp.Body.Close()
	if resp.StatusCode<200 || resp.StatusCode>=300 {
		return rawMessages, stats, fmt.Errorf("Headroom returned HTTP %d",resp.StatusCode)
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body,8<<20))
	if err != nil { return rawMessages, stats, fmt.Errorf("read Headroom response: %w",err) }
	if len(responseBody)>=8<<20 { return rawMessages, stats, errors.New("Headroom response exceeded 8 MiB") }
	var result headroomResponse
	if err := json.Unmarshal(responseBody,&result); err != nil {
		return rawMessages, stats, fmt.Errorf("decode Headroom response: %w",err)
	}
	if len(result.Messages)!=len(projected) {
		return rawMessages, stats, errors.New("Headroom response did not preserve message count")
	}

	updated := make([]json.RawMessage,len(original))
	copy(updated,original)
	stats.TokensBefore=result.TokensBefore
	stats.TokensAfter=result.TokensAfter
	stats.TokensSaved=result.TokensSaved
	for _, target := range targets {
		actual:=result.Messages[target.index]
		expected:=projected[target.index]
		if actual.Role!=expected.Role || actual.ToolCallID!=expected.ToolCallID {
			return rawMessages, HeadroomStats{}, fmt.Errorf("Headroom changed message ordering or tool identity at index %d",target.index)
		}
		text:=actual.Content
		// Never accept empty or expanded text as a compression result.
		if strings.TrimSpace(text)=="" || len(text)>=len(target.content) || text==target.content { continue }
		var message map[string]json.RawMessage
		if err:=json.Unmarshal(updated[target.index],&message);err!=nil { return rawMessages,HeadroomStats{},err }
		replacement,_:=json.Marshal(text)
		message["content"]=replacement
		updated[target.index],err=json.Marshal(message)
		if err!=nil { return rawMessages,HeadroomStats{},err }
		stats.OriginalBytes+=len(target.content)
		stats.CompressedBytes+=len(text)
		stats.MessagesChanged++
	}
	if stats.MessagesChanged==0 {
		return rawMessages,stats,errors.New("Headroom returned no smaller message content")
	}
	out,err:=json.Marshal(updated)
	if err!=nil { return rawMessages,HeadroomStats{},err }
	return out,stats,nil
}

func headroomEndpoint(base string) (string,error) {
	base=strings.TrimSpace(base)
	u,err:=url.ParseRequestURI(base)
	if err!=nil || u.Host=="" || (u.Scheme!="http" && u.Scheme!="https") || u.User!=nil {
		return "",errors.New("Headroom URL must be an http or https URL without embedded credentials")
	}
	u.Path=strings.TrimRight(u.Path,"/")+"/v1/compress"
	u.RawPath=""
	u.Fragment=""
	return u.String(),nil
}
