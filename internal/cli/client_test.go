package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type targetsOnly struct {
	godwitv1connect.UnimplementedGodwitServiceHandler
}

func (targetsOnly) ListTargets(_ context.Context, _ *connect.Request[godwitv1.ListTargetsRequest]) (*connect.Response[godwitv1.ListTargetsResponse], error) {
	return connect.NewResponse(&godwitv1.ListTargetsResponse{}), nil
}

type seenRequest struct {
	proto string
	tls   bool
	auth  string
}

func targetsServer(seen chan seenRequest) http.Handler {
	path, handler := godwitv1connect.NewGodwitServiceHandler(targetsOnly{})
	mux := http.NewServeMux()
	mux.Handle(path, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen <- seenRequest{proto: r.Proto, tls: r.TLS != nil, auth: r.Header.Get("Authorization")}
		handler.ServeHTTP(w, r)
	}))

	return mux
}

func TestTransportForScheme(t *testing.T) {
	t.Parallel()

	if _, ok := transportFor("http://godwit.internal:8474").(*http2.Transport); !ok {
		t.Fatal("http:// must keep the h2c transport")
	}
	for _, server := range []string{"https://godwit.internal", "HTTPS://godwit.internal"} {
		tr, ok := transportFor(server).(*http.Transport)
		if !ok {
			t.Fatalf("%s must not use the h2c transport: it dials port 443 in cleartext", server)
		}
		if !tr.ForceAttemptHTTP2 {
			t.Fatalf("%s: HTTP/2 must still be attempted over TLS", server)
		}
	}
}

// TestClientOverTLS guards the regression where the h2c transport dialled https:// in cleartext: a TLS
// listener rejects a plaintext dial, so the call completes only if the client really negotiated TLS.
func TestClientOverTLS(t *testing.T) {
	t.Parallel()

	seen := make(chan seenRequest, 1)
	srv := httptest.NewUnstartedServer(targetsServer(seen))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()

	tr, ok := transportFor(srv.URL).(*http.Transport)
	if !ok {
		t.Fatal("https:// must use an ordinary TLS transport")
	}
	tr.TLSClientConfig = srv.Client().Transport.(*http.Transport).TLSClientConfig
	httpClient := &http.Client{Transport: bearerTransport{token: "s3cret", next: tr}}
	client := godwitv1connect.NewGodwitServiceClient(httpClient, srv.URL)

	if _, err := client.ListTargets(context.Background(), connect.NewRequest(&godwitv1.ListTargetsRequest{})); err != nil {
		t.Fatalf("over TLS: %v", err)
	}
	got := <-seen
	if !got.tls || got.proto != "HTTP/2.0" || got.auth != "Bearer s3cret" {
		t.Fatalf("server saw %+v, want an HTTP/2 request over TLS carrying the token", got)
	}
}

func TestClientOverH2C(t *testing.T) {
	t.Parallel()

	seen := make(chan seenRequest, 1)
	srv := httptest.NewUnstartedServer(targetsServer(seen))
	protocols := new(http.Protocols)
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	srv.Config.Protocols = protocols
	srv.Start()
	defer srv.Close()

	flags := &clientFlags{server: srv.URL}
	client, err := flags.client()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListTargets(context.Background(), connect.NewRequest(&godwitv1.ListTargetsRequest{})); err != nil {
		t.Fatalf("over h2c: %v", err)
	}
	if got := <-seen; got.tls || got.proto != "HTTP/2.0" {
		t.Fatalf("server saw %+v, want cleartext HTTP/2", got)
	}
}
