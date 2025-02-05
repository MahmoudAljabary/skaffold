// Copyright 2018 Google LLC All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package transport

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
)

type bearerTransport struct {
	// Wrapped by bearerTransport.
	inner http.RoundTripper
	// Basic credentials that we exchange for bearer tokens.
	basic authn.Authenticator
	// Holds the bearer response from the token service.
	bearer *authn.Bearer
	// Registry to which we send bearer tokens.
	registry name.Registry
	// See https://tools.ietf.org/html/rfc6750#section-3
	realm string
	// See https://docs.docker.com/registry/spec/auth/token/
	service string
	scopes  []string
	// Scheme we should use, determined by ping response.
	scheme string
}

var _ http.RoundTripper = (*bearerTransport)(nil)

var portMap = map[string]string{
	"http":  "80",
	"https": "443",
}

// RoundTrip implements http.RoundTripper
func (bt *bearerTransport) RoundTrip(in *http.Request) (*http.Response, error) {
	sendRequest := func() (*http.Response, error) {
		auth, err := bt.bearer.Authorization()
		if err != nil {
			return nil, err
		}

		// http.Client handles redirects at a layer above the http.RoundTripper
		// abstraction, so to avoid forwarding Authorization headers to places
		// we are redirected, only set it when the authorization header matches
		// the registry with which we are interacting.
		// In case of redirect http.Client can use an empty Host, check URL too.
		canonicalHeaderHost := bt.canonicalAddress(in.Host)
		canonicalURLHost := bt.canonicalAddress(in.URL.Host)
		canonicalRegistryHost := bt.canonicalAddress(bt.registry.RegistryStr())
		if canonicalHeaderHost == canonicalRegistryHost || canonicalURLHost == canonicalRegistryHost {
			hdr := fmt.Sprintf("Bearer %s", auth.RegistryToken)
			in.Header.Set("Authorization", hdr)

			// When we ping() the registry, we determine whether to use http or https
			// based on which scheme was successful. That is only valid for the
			// registry server and not e.g. a separate token server or blob storage,
			// so we should only override the scheme if the host is the registry.
			in.URL.Scheme = bt.scheme
		}
		in.Header.Set("User-Agent", transportName)
		return bt.inner.RoundTrip(in)
	}

	res, err := sendRequest()
	if err != nil {
		return nil, err
	}

	// Perform a token refresh() and retry the request in case the token has expired
	if res.StatusCode == http.StatusUnauthorized {
		if err = bt.refresh(); err != nil {
			return nil, err
		}
		return sendRequest()
	}

	return res, err
}

// It's unclear which authentication flow to use based purely on the protocol,
// so we rely on heuristics and fallbacks to support as many registries as possible.
// The basic token exchange is attempted first, falling back to the oauth flow.
// If the IdentityToken is set, this indicates that we should start with the oauth flow.
func (bt *bearerTransport) refresh() error {
	first, second := bt.refreshBasic, bt.refreshOauth

	auth, err := bt.basic.Authorization()
	if err != nil {
		return err
	}
	if auth.IdentityToken != "" {
		// If the secret being stored is an identity token,
		// the Username should be set to <token>, which indicates
		// we are using an oauth flow.
		first, second = bt.refreshOauth, bt.refreshBasic
	}

	content, err := func() ([]byte, error) {
		b, err := first()
		if err != nil {
			b, err = second()
			if err != nil {
				return nil, err
			}
		}
		return b, err
	}()
	if err != nil {
		return err
	}

	// Some registries don't have "token" in the response. See #54.
	type tokenResponse struct {
		Token        string `json:"token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		// TODO: handle expiry?
	}

	var response tokenResponse
	if err := json.Unmarshal(content, &response); err != nil {
		return err
	}

	// Some registries set access_token instead of token.
	if response.AccessToken != "" {
		response.Token = response.AccessToken
	}

	// Find a token to turn into a Bearer authenticator
	var bearer authn.Bearer
	if response.Token != "" {
		bearer = authn.Bearer{Token: response.Token}
	} else {
		return fmt.Errorf("no token in bearer response:\n%s", content)
	}

	// If we obtained a refresh token from the oauth flow, use that for refresh() now.
	if response.RefreshToken != "" {
		bt.basic = authn.FromConfig(authn.AuthConfig{
			IdentityToken: response.RefreshToken,
		})
	}

	// Replace our old bearer authenticator (if we had one) with our newly refreshed authenticator.
	bt.bearer = &bearer
	return nil
}

func (bt *bearerTransport) canonicalAddress(host string) (address string) {
	// The host may be any one of:
	// - hostname
	// - hostname:port
	// - ipv4
	// - ipv4:port
	// - ipv6
	// - [ipv6]:port
	// As net.SplitHostPort returns an error if the host does not contain a port, we should only attempt
	// to call it when we know that the address contains a port
	if strings.Count(host, ":") == 1 || (strings.Count(host, ":") >= 2 && strings.Contains(host, "]:")) {
		hostname, port, err := net.SplitHostPort(host)
		if err != nil {
			return host
		}
		if port == "" {
			port = portMap[bt.scheme]
		}

		return net.JoinHostPort(hostname, port)
	}

	return net.JoinHostPort(host, portMap[bt.scheme])
}

// https://docs.docker.com/registry/spec/auth/oauth/
func (bt *bearerTransport) refreshOauth() ([]byte, error) {
	auth, err := bt.basic.Authorization()
	if err != nil {
		return nil, err
	}

	u, err := url.Parse(bt.realm)
	if err != nil {
		return nil, err
	}

	v := url.Values{}
	v.Set("scope", strings.Join(bt.scopes, " "))
	v.Set("service", bt.service)
	v.Set("client_id", transportName)
	if auth.IdentityToken != "" {
		v.Set("grant_type", "refresh_token")
		v.Set("refresh_token", auth.IdentityToken)
	} else if auth.Username != "" && auth.Password != "" {
		v.Set("grant_type", "password")
		v.Set("username", auth.Username)
		v.Set("password", auth.Password)
		v.Set("access_type", "offline")
	}

	client := http.Client{Transport: bt.inner}
	resp, err := client.PostForm(u.String(), v)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := CheckError(resp, http.StatusOK); err != nil {
		return nil, err
	}

	return ioutil.ReadAll(resp.Body)
}

// https://docs.docker.com/registry/spec/auth/token/
func (bt *bearerTransport) refreshBasic() ([]byte, error) {
	u, err := url.Parse(bt.realm)
	if err != nil {
		return nil, err
	}
	b := &basicTransport{
		inner:  bt.inner,
		auth:   bt.basic,
		target: u.Host,
	}
	client := http.Client{Transport: b}

	u.RawQuery = url.Values{
		"scope":   bt.scopes,
		"service": []string{bt.service},
	}.Encode()

	resp, err := client.Get(u.String())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := CheckError(resp, http.StatusOK); err != nil {
		return nil, err
	}

	return ioutil.ReadAll(resp.Body)
}
