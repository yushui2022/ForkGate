// Command forkgate-agent is a deterministic local harness for a running
// ForkGate instance. It simulates an orchestrator after an external sandbox
// backend has already forked: the /fork call below only registers that lineage
// and obtains child identities; it never creates a sandbox itself.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

type Action struct {
	Name   string
	Method string
	URL    string
	Body   string
}

type ToolPlan struct {
	Name    string
	Actions []Action
}

type treeResponse struct {
	TreeID     string `json:"tree_id"`
	RootBranch struct {
		ID string `json:"branch_id"`
	} `json:"root_branch"`
	Token string `json:"token"`
}

type forkResponse struct {
	Children []struct {
		BranchID string `json:"branch_id"`
		Token    string `json:"token"`
	} `json:"children"`
}

type stagedResponse struct {
	Staged []struct {
		ID     string `json:"staged_id"`
		Status string `json:"status"`
		Method string `json:"method"`
		Host   string `json:"host"`
	} `json:"staged"`
}

type commitResponse struct {
	BranchID string `json:"branch_id"`
	Status   string `json:"status"`
	Items    []struct {
		ID             string `json:"staged_id"`
		Seq            int64  `json:"seq"`
		Status         string `json:"status"`
		ResponseStatus int    `json:"response_status"`
	} `json:"items"`
}

type child struct {
	id    string
	token string
}

func main() {
	control := flag.String("control", envOr("FORKGATE_CONTROL_URL", "http://127.0.0.1:7070"), "ForkGate control API URL")
	proxy := flag.String("proxy", envOr("FORKGATE_PROXY_URL", "http://127.0.0.1:3128"), "ForkGate explicit proxy URL")
	adminToken := flag.String("admin-token", os.Getenv("FORKGATE_ADMIN_TOKEN"), "ForkGate control bearer token")
	flag.Parse()
	if *adminToken == "" {
		fatal(errors.New("-admin-token or FORKGATE_ADMIN_TOKEN is required"))
	}
	if err := run(strings.TrimRight(*control, "/"), strings.TrimRight(*proxy, "/"), *adminToken); err != nil {
		fatal(err)
	}
}

func run(controlURL, proxyURL, adminToken string) error {
	var reads, writes atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			reads.Add(1)
			w.Header().Set("Content-Type", "text/plain")
			_, _ = io.WriteString(w, "agent-read-ok")
		default:
			writes.Add(1)
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer upstream.Close()

	controlClient := &http.Client{Timeout: 15 * time.Second}
	proxyURLParsed, err := url.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("parse proxy URL: %w", err)
	}
	proxyClient := &http.Client{
		Timeout:   15 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(proxyURLParsed)},
	}

	var tree treeResponse
	if err := controlJSON(controlClient, http.MethodPost, controlURL+"/v1/trees", adminToken, nil, &tree); err != nil {
		return fmt.Errorf("create tree: %w", err)
	}
	var fork forkResponse
	if err := controlJSON(controlClient, http.MethodPost, controlURL+"/v1/branches/"+tree.RootBranch.ID+"/fork", adminToken, map[string]int{"count": 2}, &fork); err != nil {
		return fmt.Errorf("fork tree: %w", err)
	}
	if len(fork.Children) != 2 {
		return fmt.Errorf("fork returned %d children, want 2", len(fork.Children))
	}
	children := []child{{fork.Children[0].BranchID, fork.Children[0].Token}, {fork.Children[1].BranchID, fork.Children[1].Token}}

	plan := ToolPlan{
		Name: "fork-lineage-multi-call",
		Actions: []Action{
			{Name: "child1.read", Method: http.MethodGet, URL: upstream.URL + "/read/child1"},
			{Name: "child1.write.1", Method: http.MethodPost, URL: upstream.URL + "/write/child1/1", Body: `{"from":"child-1","seq":1}`},
			{Name: "child1.write.2", Method: http.MethodPost, URL: upstream.URL + "/write/child1/2", Body: `{"from":"child-1","seq":2}`},
			{Name: "child2.read", Method: http.MethodGet, URL: upstream.URL + "/read/child2"},
			{Name: "child2.write.1", Method: http.MethodPost, URL: upstream.URL + "/write/child2/1", Body: `{"from":"child-2","seq":1}`},
		},
	}
	fmt.Printf("harness_plan=%s tree_id=%s registered_children=%d upstream=%s\n", plan.Name, tree.TreeID, len(children), upstream.URL)

	child1Read, child1ReadBody, err := doTool(proxyClient, children[0].token, plan.Actions[0])
	if err != nil {
		return fmt.Errorf("execute %s: %w", plan.Actions[0].Name, err)
	}
	child1Write1, _, err := doTool(proxyClient, children[0].token, plan.Actions[1])
	if err != nil {
		return fmt.Errorf("execute %s: %w", plan.Actions[1].Name, err)
	}
	child1Write2, _, err := doTool(proxyClient, children[0].token, plan.Actions[2])
	if err != nil {
		return fmt.Errorf("execute %s: %w", plan.Actions[2].Name, err)
	}
	child2Read, _, err := doTool(proxyClient, children[1].token, plan.Actions[3])
	if err != nil {
		return fmt.Errorf("execute %s: %w", plan.Actions[3].Name, err)
	}
	child2Write1, _, err := doTool(proxyClient, children[1].token, plan.Actions[4])
	if err != nil {
		return fmt.Errorf("execute %s: %w", plan.Actions[4].Name, err)
	}
	var staged1, staged2 stagedResponse
	if err := controlJSON(controlClient, http.MethodGet, controlURL+"/v1/branches/"+children[0].id+"/staged", adminToken, nil, &staged1); err != nil {
		return fmt.Errorf("list child-1 staged writes: %w", err)
	}
	if err := controlJSON(controlClient, http.MethodGet, controlURL+"/v1/branches/"+children[1].id+"/staged", adminToken, nil, &staged2); err != nil {
		return fmt.Errorf("list child-2 staged writes: %w", err)
	}

	abortReq, err := http.NewRequestWithContext(context.Background(), http.MethodPost, controlURL+"/v1/branches/"+children[0].id+"/abort", nil)
	if err != nil {
		return err
	}
	abortReq.Header.Set("Authorization", "Bearer "+adminToken)
	abortResp, err := controlClient.Do(abortReq)
	if err != nil {
		return fmt.Errorf("abort child: %w", err)
	}
	_ = abortResp.Body.Close()
	terminal, _, err := doTool(proxyClient, children[0].token, Action{Name: "child1.after_abort", Method: http.MethodPost, URL: upstream.URL + "/after-abort"})
	if err != nil {
		return fmt.Errorf("execute post-abort check: %w", err)
	}
	child2ReadAfterAbort, _, err := doTool(proxyClient, children[1].token, Action{Name: "child2.read_after_abort", Method: http.MethodGet, URL: upstream.URL + "/read/after-abort"})
	if err != nil {
		return fmt.Errorf("execute child2 read after abort: %w", err)
	}
	child2WriteAfterAbort, _, err := doTool(proxyClient, children[1].token, Action{Name: "child2.write_after_abort", Method: http.MethodPost, URL: upstream.URL + "/write/after-abort", Body: `{"from":"child-2","seq":2}`})
	if err != nil {
		return fmt.Errorf("execute child2 write after abort: %w", err)
	}
	var staged1AfterAbort, staged2AfterAbort stagedResponse
	if err := controlJSON(controlClient, http.MethodGet, controlURL+"/v1/branches/"+children[0].id+"/staged", adminToken, nil, &staged1AfterAbort); err != nil {
		return fmt.Errorf("list child-1 staged writes after abort: %w", err)
	}
	if err := controlJSON(controlClient, http.MethodGet, controlURL+"/v1/branches/"+children[1].id+"/staged", adminToken, nil, &staged2AfterAbort); err != nil {
		return fmt.Errorf("list child-2 staged writes after abort: %w", err)
	}
	var committed commitResponse
	if err := controlJSON(controlClient, http.MethodPost, controlURL+"/v1/branches/"+children[1].id+"/commit", adminToken, nil, &committed); err != nil {
		return fmt.Errorf("commit child-2: %w", err)
	}
	child2AfterCommit, _, err := doTool(proxyClient, children[1].token, Action{Name: "child2.after_commit", Method: http.MethodGet, URL: upstream.URL + "/read/after-commit"})
	if err != nil {
		return fmt.Errorf("execute post-commit check: %w", err)
	}
	var staged2AfterCommit stagedResponse
	if err := controlJSON(controlClient, http.MethodGet, controlURL+"/v1/branches/"+children[1].id+"/staged", adminToken, nil, &staged2AfterCommit); err != nil {
		return fmt.Errorf("list child-2 staged writes after commit: %w", err)
	}

	stagedSummary := func(items stagedResponse) string {
		if len(items.Staged) == 0 {
			return "none"
		}
		return fmt.Sprintf("%d:%s", len(items.Staged), items.Staged[0].Status)
	}
	fmt.Printf("child1 read=%d body=%q writes=[%d,%d] staged_before=%s; child2 read=%d write=%d staged_before=%s; child1_abort=%d child1_after_abort=%d child1_staged=%s; child2_read_after_abort=%d child2_write_after_abort=%d child2_staged_before_commit=%s commit=%s/%d child2_after_commit=%d child2_staged_after_commit=%s upstream_reads=%d upstream_writes=%d\n", child1Read.StatusCode, string(child1ReadBody), child1Write1.StatusCode, child1Write2.StatusCode, stagedSummary(staged1), child2Read.StatusCode, child2Write1.StatusCode, stagedSummary(staged2), abortResp.StatusCode, terminal.StatusCode, stagedSummary(staged1AfterAbort), child2ReadAfterAbort.StatusCode, child2WriteAfterAbort.StatusCode, stagedSummary(staged2AfterAbort), committed.Status, len(committed.Items), child2AfterCommit.StatusCode, stagedSummary(staged2AfterCommit), reads.Load(), writes.Load())
	if child1Read.StatusCode != http.StatusOK || child1Write1.StatusCode != http.StatusAccepted || child1Write2.StatusCode != http.StatusAccepted || child2Read.StatusCode != http.StatusOK || child2Write1.StatusCode != http.StatusAccepted || abortResp.StatusCode != http.StatusNoContent || terminal.StatusCode != http.StatusGone || child2ReadAfterAbort.StatusCode != http.StatusOK || child2WriteAfterAbort.StatusCode != http.StatusAccepted || committed.Status != "committed" || len(committed.Items) != 2 || child2AfterCommit.StatusCode != http.StatusGone {
		return fmt.Errorf("unexpected agent flow status")
	}
	if reads.Load() != 3 || writes.Load() != 2 {
		return fmt.Errorf("unexpected upstream counts: reads=%d writes=%d", reads.Load(), writes.Load())
	}
	return nil
}

func doTool(client *http.Client, token string, action Action) (*http.Response, []byte, error) {
	var body io.Reader
	if action.Body != "" {
		body = bytes.NewBufferString(action.Body)
	}
	req, err := http.NewRequestWithContext(context.Background(), action.Method, action.URL, body)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Proxy-Authorization", "Bearer "+token)
	if action.Body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	data, readErr := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp, data, readErr
}

func controlJSON(client *http.Client, method, endpoint, token string, requestBody any, target any) error {
	var body io.Reader
	if requestBody != nil {
		encoded, err := json.Marshal(requestBody)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, endpoint, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if requestBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s returned %d: %s", method, endpoint, resp.StatusCode, strings.TrimSpace(string(data)))
	}
	if target == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(target)
}

func envOr(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "forkgate-agent: %v\n", err)
	os.Exit(1)
}
