package optimizer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCompressWithHeadroomPreservesToolIdentityAndOnlyAppliesSmallerText(t *testing.T) {
	var called bool
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.URL.Path != "/v1/compress" { t.Errorf("Headroom path = %q", r.URL.Path) }
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode Headroom request: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		messages, _ := request["messages"].([]any)
		if request["model"] != "test-model" || len(messages) != 3 {
			t.Errorf("unexpected Headroom projection: %#v", request)
		}
		if _, ok := request["config"]; ok {
			t.Errorf("user-message compression should be disabled: %#v", request["config"])
		}
		last, _ := messages[2].(map[string]any)
		if last["role"] != "tool" || last["tool_call_id"] != "call-1" {
			t.Errorf("tool identity was not projected: %#v", last)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"messages": []map[string]string{
				{"role":"user","content":"What happened?"},
				{"role":"assistant","content":""},
				{"role":"tool","tool_call_id":"call-1","content":"3 files changed"},
			},
			"tokens_before":100,"tokens_after":60,"tokens_saved":40,
		})
	}))
	defer proxy.Close()

	originalValue := []map[string]any{
		{"role":"user","content":"What happened?"},
		{"role":"assistant","content":"","tool_calls":[]any{map[string]any{"id":"call-1","type":"function","function":map[string]any{"name":"bash","arguments":"{}"}}}},
		{"role":"tool","tool_call_id":"call-1","content":"file a changed\nfile b changed\nfile c changed"},
	}
	original, _ := json.Marshal(originalValue)
	compressed, stats, err := CompressWithHeadroom(context.Background(),proxy.URL,"test-model",original,time.Second,false)
	if err != nil { t.Fatalf("CompressWithHeadroom() error = %v",err) }
	if !called { t.Fatal("Headroom endpoint was not called") }

	var before, after []map[string]any
	if err:=json.Unmarshal(original,&before);err!=nil {t.Fatal(err)}
	if err:=json.Unmarshal(compressed,&after);err!=nil {t.Fatal(err)}
	if after[0]["content"]!=before[0]["content"] {t.Fatalf("user message changed although user compression is disabled: %#v",after[0])}
	if after[2]["content"]!="3 files changed" {t.Fatalf("tool output was not compressed: %#v",after[2])}
	toolCalls,_:=after[1]["tool_calls"].([]any)
	if len(toolCalls)!=1 {t.Fatalf("assistant tool call metadata was lost: %#v",after[1])}
	if stats.MessagesChanged!=1 || stats.TokensSaved!=40 || stats.OriginalBytes<=stats.CompressedBytes {
		t.Fatalf("unexpected Headroom stats: %#v",stats)
	}
}

func TestCompressWithHeadroomCanExplicitlyCompressUserMessages(t *testing.T) {
	proxy:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
		var request map[string]any
		_ = json.NewDecoder(r.Body).Decode(&request)
		config,ok:=request["config"].(map[string]any)
		if !ok || config["compress_user_messages"]!=true {
			t.Errorf("compress_user_messages opt-in missing: %#v",request["config"])
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"messages":[]map[string]string{{"role":"user","content":"short"}}})
	}))
	defer proxy.Close()
	original,_:=json.Marshal([]map[string]string{{"role":"user","content":"a longer user prompt to compress"}})
	compressed,stats,err:=CompressWithHeadroom(context.Background(),proxy.URL,"test-model",original,time.Second,true)
	if err!=nil {t.Fatalf("CompressWithHeadroom() error = %v",err)}
	if stats.MessagesChanged!=1 || !strings.Contains(string(compressed),"short") {
		t.Fatalf("user message was not compressed: %s %#v",compressed,stats)
	}
}

func TestCompressWithHeadroomFailsOpenOnHTTPOrProjectionMismatch(t *testing.T) {
	cases:=[]struct{name string;status int;messages any}{
		{name:"upstream error",status:http.StatusServiceUnavailable,messages:nil},
		{name:"message count mismatch",status:http.StatusOK,messages:[]map[string]string{}},
		{name:"role mismatch",status:http.StatusOK,messages:[]map[string]string{{"role":"assistant","content":"changed"}}},
	}
	original,_:=json.Marshal([]map[string]string{{"role":"tool","tool_call_id":"call-1","content":"keep original tool output"}})
	for _,tc:=range cases {
		t.Run(tc.name,func(t *testing.T){
			proxy:=httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,r *http.Request){
				w.WriteHeader(tc.status)
				if tc.status>=200 && tc.status<300 { _=json.NewEncoder(w).Encode(map[string]any{"messages":tc.messages}) }
			}))
			defer proxy.Close()
			compressed,_,err:=CompressWithHeadroom(context.Background(),proxy.URL,"test-model",original,time.Second,false)
			if err==nil {t.Fatal("expected Headroom error")}
			if string(compressed)!=string(original) {t.Fatalf("failure did not retain original payload: %s",compressed)}
		})
	}
}

func TestHeadroomEndpointRejectsCredentialsAndInvalidSchemes(t *testing.T) {
	for _,value:=range []string{"ftp://example.com","https://user:pass@example.com","not a URL"} {
		if _,err:=headroomEndpoint(value);err==nil { t.Errorf("headroomEndpoint(%q) unexpectedly succeeded",value) }
	}
	if endpoint,err:=headroomEndpoint("http://localhost:8787/base/");err!=nil || endpoint!="http://localhost:8787/base/v1/compress" {
		t.Fatalf("headroomEndpoint() = %q, %v",endpoint,err)
	}
}
