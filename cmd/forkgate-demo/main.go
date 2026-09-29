// Command forkgate-demo runs a small local, HTTP-only walkthrough of the
// Phase 2 tree/token/staging path. It simulates the control-plane registration
// that follows an external sandbox fork; it does not create a sandbox.
// It uses only loopback httptest servers and leaves its SQLite database under
// G:\DevCache\Temp\ForkGate for inspection.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/yushui2022/ForkGate/internal/api"
	"github.com/yushui2022/ForkGate/internal/branches"
	"github.com/yushui2022/ForkGate/internal/mitm"
	"github.com/yushui2022/ForkGate/internal/proxy"
	"github.com/yushui2022/ForkGate/internal/store"
)

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
		Status string `json:"status"`
	} `json:"staged"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "forkgate-demo: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	root, err := os.MkdirTemp(`G:\DevCache\Temp\ForkGate`, "demo-")
	if err != nil {
		return err
	}
	db, err := store.Open(filepath.Join(root, "forkgate.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	ca, err := mitm.LoadOrCreate(filepath.Join(root, "ca"))
	if err != nil {
		return err
	}
	manager := branches.New(db)
	if err := manager.SetStagingKey([]byte("01234567890123456789012345678901")); err != nil {
		return err
	}
	p := proxy.New(db, ca)
	p.SetBranchManager(manager)
	control := api.New(db, ca, "demo-admin")
	control.SetBranchManager(manager)
	proxyServer := httptest.NewServer(p)
	defer proxyServer.Close()
	controlServer := httptest.NewServer(control.Handler())
	defer controlServer.Close()

	var reads, writes atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			reads.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("read-through"))
			return
		}
		writes.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer upstream.Close()

	var tree treeResponse
	if err := controlJSON(controlServer.Client(), http.MethodPost, controlServer.URL+"/v1/trees", nil, &tree); err != nil {
		return err
	}
	var fork forkResponse
	forkBody := map[string]int{"count": 3}
	if err := controlJSON(controlServer.Client(), http.MethodPost, controlServer.URL+"/v1/branches/"+tree.RootBranch.ID+"/fork", forkBody, &fork); err != nil {
		return err
	}
	if len(fork.Children) != 3 {
		return fmt.Errorf("fork returned %d children", len(fork.Children))
	}
	child := fork.Children[0]
	proxyURL, _ := url.Parse(proxyServer.URL)
	client := &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL)}}

	readReq, _ := http.NewRequest(http.MethodGet, upstream.URL+"/read", nil)
	readReq.Header.Set("Proxy-Authorization", "Bearer "+child.Token)
	readResp, err := client.Do(readReq)
	if err != nil {
		return err
	}
	readBody, _ := io.ReadAll(readResp.Body)
	_ = readResp.Body.Close()

	writeReq, _ := http.NewRequest(http.MethodPost, upstream.URL+"/write", bytes.NewReader(nil))
	writeReq.Header.Set("Proxy-Authorization", "Bearer "+child.Token)
	writeResp, err := client.Do(writeReq)
	if err != nil {
		return err
	}
	_ = writeResp.Body.Close()

	oldReq, _ := http.NewRequest(http.MethodPost, upstream.URL+"/old", nil)
	oldReq.Header.Set("Proxy-Authorization", "Bearer "+tree.Token)
	oldResp, err := client.Do(oldReq)
	if err != nil {
		return err
	}
	_ = oldResp.Body.Close()

	abortReq, _ := http.NewRequest(http.MethodPost, controlServer.URL+"/v1/branches/"+child.BranchID+"/abort", nil)
	abortReq.Header.Set("Authorization", "Bearer demo-admin")
	abortResp, err := controlServer.Client().Do(abortReq)
	if err != nil {
		return err
	}
	_ = abortResp.Body.Close()
	var staged stagedResponse
	if err := controlJSON(controlServer.Client(), http.MethodGet, controlServer.URL+"/v1/branches/"+child.BranchID+"/staged", nil, &staged); err != nil {
		return err
	}

	terminalReq, _ := http.NewRequest(http.MethodPost, upstream.URL+"/after-abort", nil)
	terminalReq.Header.Set("Proxy-Authorization", "Bearer "+child.Token)
	terminalResp, err := client.Do(terminalReq)
	if err != nil {
		return err
	}
	_ = terminalResp.Body.Close()

	discardedStatus := "none"
	if len(staged.Staged) > 0 {
		discardedStatus = staged.Staged[0].Status
	}
	fmt.Printf("tree=%s children=%d read_status=%d read_body=%q staged_status=%d upstream_writes=%d stale_parent_status=%d abort_status=%d discarded_status=%s terminal_status=%d\n", tree.TreeID, len(fork.Children), readResp.StatusCode, string(readBody), writeResp.StatusCode, writes.Load(), oldResp.StatusCode, abortResp.StatusCode, discardedStatus, terminalResp.StatusCode)
	fmt.Printf("demo data was kept at %s\n", root)
	return nil
}

func controlJSON(client *http.Client, method, endpoint string, body any, target any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer demo-admin")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		data, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s: %s", method, endpoint, string(data))
	}
	return json.NewDecoder(resp.Body).Decode(target)
}
