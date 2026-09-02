package cli

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"

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
	cmd.Flags().StringVar(&f.server, "server", os.Getenv("GODWIT_SERVER"), "godwit service URL (env GODWIT_SERVER)")
	cmd.Flags().StringVar(&f.token, "token", os.Getenv("GODWIT_TOKEN"), "bearer token (env GODWIT_TOKEN)")
	cmd.Flags().BoolVar(&f.json, "json", false, "print the raw JSON response")
	configKeys(cmd, "server")
}

func (f *clientFlags) client() (godwitv1connect.GodwitServiceClient, error) {
	if f.server == "" {
		return nil, errors.New("--server (or GODWIT_SERVER, or server in godwit.yaml) is required")
	}
	var transport http.RoundTripper = &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer

			return d.DialContext(ctx, network, addr)
		},
	}
	if f.token != "" {
		transport = bearerTransport{token: f.token, next: transport}
	}

	return godwitv1connect.NewGodwitServiceClient(&http.Client{Transport: transport}, f.server), nil
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
