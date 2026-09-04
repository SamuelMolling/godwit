package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	godwitv1 "github.com/SamuelMolling/godwit/gen/godwit/v1"
	"github.com/SamuelMolling/godwit/gen/godwit/v1/godwitv1connect"
)

type clientFlags struct {
	server string
	token  string
	json   bool
}

func (f *clientFlags) register(cmd *cobra.Command) {
	f.registerServer(cmd)
	cmd.Flags().BoolVar(&f.json, "json", false, "print the raw JSON response")
}

// registerServer adds the connection flags alone, for a command that also works with no server.
func (f *clientFlags) registerServer(cmd *cobra.Command) {
	cmd.Flags().StringVar(&f.server, "server", os.Getenv("GODWIT_SERVER"), "godwit service URL (env GODWIT_SERVER)")
	cmd.Flags().StringVar(&f.token, "token", os.Getenv("GODWIT_TOKEN"), "bearer token (env GODWIT_TOKEN)")
	configKeys(cmd, "server")
}

func (f *clientFlags) client() (godwitv1connect.GodwitServiceClient, error) {
	if f.server == "" {
		return nil, errors.New("--server (or GODWIT_SERVER, or server in godwit.yaml) is required")
	}

	return f.dial(), nil
}

// dial builds the client for a caller that has already decided the server is set.
func (f *clientFlags) dial() godwitv1connect.GodwitServiceClient {
	transport := transportFor(f.server)
	if f.token != "" {
		transport = bearerTransport{token: f.token, next: transport}
	}

	return godwitv1connect.NewGodwitServiceClient(&http.Client{Transport: transport}, f.server)
}

// transportFor picks the transport the server URL's scheme needs. http2.Transport calls DialTLSContext
// for https:// as well as http://, so an h2c transport there would dial port 443 in cleartext.
func transportFor(server string) http.RoundTripper {
	if strings.HasPrefix(strings.ToLower(server), "https://") {
		return http.DefaultTransport.(*http.Transport).Clone()
	}

	return &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, network, addr)
		},
	}
}

type remoteFunc func(cmd *cobra.Command, client godwitv1connect.GodwitServiceClient, args []string) error

func (f *clientFlags) runE(fn remoteFunc) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		client, err := f.client()
		if err != nil {
			return err
		}

		return apiError(fn(cmd, client, args))
	}
}

// ExitPlanRefused is the exit code when the service refuses to bind a migration set to a stored plan.
const ExitPlanRefused = 3

type exitError struct {
	code int
	msg  string
}

func (e exitError) Error() string { return e.msg }

func apiError(err error) error {
	var cerr *connect.Error
	if !errors.As(err, &cerr) {
		return err
	}
	for _, d := range cerr.Details() {
		switch d.Type() {
		case string((&godwitv1.PlanStale{}).ProtoReflect().Descriptor().FullName()),
			string((&godwitv1.PlanRequired{}).ProtoReflect().Descriptor().FullName()):
			return exitError{code: ExitPlanRefused, msg: cerr.Message()}
		}
	}

	return errors.New(cerr.Message())
}

func (f *clientFlags) print(cmd *cobra.Command, msg proto.Message, human string) {
	if f.json {
		fmt.Fprintln(cmd.OutOrStdout(), protojson.MarshalOptions{}.Format(msg))

		return
	}
	fmt.Fprintln(cmd.OutOrStdout(), human)
}

type bearerTransport struct {
	token string
	next  http.RoundTripper
}

// RoundTrip sends the request with the bearer token attached.
func (t bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", "Bearer "+t.token)

	return t.next.RoundTrip(req)
}
