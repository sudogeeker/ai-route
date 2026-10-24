package warp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterCloudflareDevice(t *testing.T) {
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests = append(requests, r.Method+" "+r.URL.Path)
		if r.Header.Get("CF-Client-Version") != "a-6.10-2158" || r.Header.Get("User-Agent") != "okhttp/3.12.1" {
			t.Errorf("wrong Cloudflare client headers: %v", r.Header)
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/reg":
			var body map[string]string
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			key, err := base64.StdEncoding.DecodeString(body["key"])
			if err != nil || len(key) != 32 || body["model"] != "PC" || body["locale"] != "en_US" {
				t.Errorf("invalid registration: %v %v", body, err)
			}
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "device-id", "token": "device-token"}})
		case r.Method == http.MethodPatch && r.URL.Path == "/reg/device-id":
			if r.Header.Get("Authorization") != "Bearer device-token" {
				t.Errorf("missing registration token: %v", r.Header)
			}
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "device-id"}})
		case r.Method == http.MethodGet && r.URL.Path == "/reg/device-id":
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{
				"id": "device-id", "token": "device-token", "warp_enabled": true,
				"config": map[string]any{
					"interface": map[string]any{"addresses": map[string]any{"v4": "172.16.0.2", "v6": "2606:4700:110::2"}},
					"peers": []any{map[string]any{
						"public_key": defaultPeer,
						"endpoint": map[string]any{
							"host":  "engage.cloudflareclient.com:2408",
							"v4":    "162.159.192.4:0",
							"v6":    "[2606:4700:d0::a29f:c004]:0",
							"ports": []any{2408, 500},
						},
					}},
				},
			}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	r := &registrar{base: server.URL, client: server.Client(), minGap: 0}
	reg, err := r.register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if reg.ID != "device-id" || reg.Token != "device-token" || reg.AddressV6 != "2606:4700:110::2" || reg.PeerPublicKey != defaultPeer || reg.Endpoint != "162.159.192.4:2408" {
		t.Fatalf("unexpected registration: %+v", reg)
	}
	if len(reg.Endpoints) < 4 || reg.Endpoints[1] != "[2606:4700:d0::a29f:c004]:2408" || reg.Endpoints[2] != "engage.cloudflareclient.com:2408" {
		t.Fatalf("unexpected endpoint priority: %v", reg.Endpoints)
	}
	if reg.PrivateKey == "" || reg.PublicKey == "" || reg.PrivateKey == reg.PublicKey {
		t.Fatalf("invalid keypair: %+v", reg)
	}
	if got := strings.Join(requests, ","); got != "POST /reg,PATCH /reg/device-id,GET /reg/device-id" {
		t.Fatalf("unexpected API sequence: %s", got)
	}
}

func TestRegisterRequiresEnabledWARP(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "id", "token": "token"}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"result": map[string]any{"id": "id", "warp_enabled": false}})
		}
	}))
	defer server.Close()
	r := &registrar{base: server.URL, client: server.Client()}
	if _, err := r.register(context.Background()); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("disabled WARP device was accepted: %v", err)
	}
}
