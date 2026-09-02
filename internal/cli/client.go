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
}

func (f *clientFlags) client() (godwitv1connect.GodwitServiceClient, error) {
	if f.server == "" {
		return nil, errors.New("--server (or GODWIT_SERVER) is required")
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

func apiError(err error) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return errors.New(cerr.Message())
	}

	return err
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
