package main

import (
	"bufio"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/spf13/cobra"

	incus "github.com/lxc/incus/v6/client"
	cli "github.com/lxc/incus/v6/internal/cmd"
	"github.com/lxc/incus/v6/internal/i18n"
	"github.com/lxc/incus/v6/internal/ports"
	internalUtil "github.com/lxc/incus/v6/internal/util"
	"github.com/lxc/incus/v6/shared/api"
	config "github.com/lxc/incus/v6/shared/cliconfig"
	localtls "github.com/lxc/incus/v6/shared/tls"
	"github.com/lxc/incus/v6/shared/util"
)

type cmdRemote struct {
	global *cmdGlobal
}

type remoteColumn struct {
	Name string
	Data func(string, config.Remote) string
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemote) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("remote")
	cmd.Short = i18n.G("Manage the list of remote servers")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Manage the list of remote servers`))

	// Add
	remoteAddCmd := cmdRemoteAdd{global: c.global, remote: c}
	cmd.AddCommand(remoteAddCmd.Command())

	// Generate certificate
	remoteGenerateCertificateCmd := cmdRemoteGenerateCertificate{global: c.global, remote: c}
	cmd.AddCommand(remoteGenerateCertificateCmd.Command())

	// Get default
	remoteGetDefaultCmd := cmdRemoteGetDefault{global: c.global, remote: c}
	cmd.AddCommand(remoteGetDefaultCmd.Command())

	// List
	remoteListCmd := cmdRemoteList{global: c.global, remote: c}
	cmd.AddCommand(remoteListCmd.Command())

	if runtime.GOOS != "windows" {
		// Proxy
		remoteProxyCmd := cmdRemoteProxy{global: c.global, remote: c}
		cmd.AddCommand(remoteProxyCmd.Command())
	}

	// Rename
	remoteRenameCmd := cmdRemoteRename{global: c.global, remote: c}
	cmd.AddCommand(remoteRenameCmd.Command())

	// Remove
	remoteRemoveCmd := cmdRemoteRemove{global: c.global, remote: c}
	cmd.AddCommand(remoteRemoveCmd.Command())

	// Set default
	remoteSwitchCmd := cmdRemoteSwitch{global: c.global, remote: c}
	cmd.AddCommand(remoteSwitchCmd.Command())

	// Set URL
	remoteSetURLCmd := cmdRemoteSetURL{global: c.global, remote: c}
	cmd.AddCommand(remoteSetURLCmd.Command())

	// Get client certificate
	remoteGetClientCertificateCmd := cmdRemoteGetClientCertificate{global: c.global, remote: c}
	cmd.AddCommand(remoteGetClientCertificateCmd.Command())

	// Get client token
	remoteGetClientTokenCmd := cmdRemoteGetClientToken{global: c.global, remote: c}
	cmd.AddCommand(remoteGetClientTokenCmd.Command())

	// Workaround for subcommand usage errors. See: https://github.com/spf13/cobra/issues/706
	cmd.Args = cobra.NoArgs
	cmd.Run = func(cmd *cobra.Command, _ []string) { _ = cmd.Usage() }
	return cmd
}

// Add.
type cmdRemoteAdd struct {
	global *cmdGlobal
	remote *cmdRemote

	flagAcceptCert bool
	flagToken      string
	flagPublic     bool
	flagProtocol   string
	flagAuthType   string
	flagProject    string
	flagKeepAlive  int
	flagCredHelper string
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteAdd) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("add", i18n.G("[<remote>] <IP|FQDN|URL|token>"))
	cmd.Short = i18n.G("Add new remote servers")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Add new remote servers

URL for remote resources must be HTTPS (https://).

Basic authentication can be used when combined with the "simplestreams" protocol:
  incus remote add some-name https://LOGIN:PASSWORD@example.com/some/path --protocol=simplestreams
`))

	cmd.RunE = c.Run
	cmd.Flags().BoolVar(&c.flagAcceptCert, "accept-certificate", false, i18n.G("Accept certificate"))
	cmd.Flags().StringVar(&c.flagToken, "token", "", i18n.G("Remote trust token")+"``")
	cmd.Flags().StringVar(&c.flagProtocol, "protocol", "", i18n.G("Server protocol (incus, oci or simplestreams)")+"``")
	cmd.Flags().StringVar(&c.flagAuthType, "auth-type", "", i18n.G("Server authentication type (tls or oidc)")+"``")
	cmd.Flags().BoolVar(&c.flagPublic, "public", false, i18n.G("Public image server"))
	cmd.Flags().StringVar(&c.flagProject, "project", "", i18n.G("Project to use for the remote")+"``")
	cmd.Flags().IntVar(&c.flagKeepAlive, "keepalive", 0, i18n.G("Maintain remote connection for faster commands")+"``")
	cmd.Flags().StringVar(&c.flagCredHelper, "credentials-helper", "", i18n.G("Binary helper for retrieving credentials")+"``")

	return cmd
}

func (c *cmdRemoteAdd) findProject(d incus.InstanceServer, project string) (string, error) {
	if project == "" {
		// Check if we can pull a list of projects.
		if d.HasExtension("projects") {
			// Retrieve the allowed projects.
			names, err := d.GetProjectNames()
			if err != nil {
				return "", err
			}

			if len(names) == 0 {
				// If no allowed projects, just keep it to the default.
				return "", nil
			} else if len(names) == 1 {
				// If only a single project, use that.
				return names[0], nil
			}

			// Deal with multiple projects.
			if slices.Contains(names, api.ProjectDefaultName) {
				// If we have access to the default project, use it.
				return "", nil
			}

			// Let's ask the user.
			fmt.Println(i18n.G("Available projects:"))
			for _, name := range names {
				fmt.Println(" - " + name)
			}

			return c.global.asker.AskChoice(i18n.G("Name of the project to use for this remote:")+" ", names, "")
		}

		return "", nil
	}

	_, _, err := d.GetProject(project)
	if err != nil {
		return "", err
	}

	return project, nil
}

func (c *cmdRemoteAdd) runToken(server string, token string, rawToken *api.CertificateAddToken) error {
	conf := c.global.conf

	if !conf.HasClientCertificate() {
		fmt.Fprint(os.Stderr, i18n.G("Generating a client certificate. This may take a minute...")+"\n")
		err := conf.GenerateClientCertificate()
		if err != nil {
			return err
		}
	}

	for _, addr := range rawToken.Addresses {
		addr = fmt.Sprintf("https://%s", addr)

		err := c.addRemoteFromToken(addr, server, token, rawToken.Fingerprint)
		if err != nil {
			if api.StatusErrorCheck(err, http.StatusServiceUnavailable) {
				continue
			}

			return err
		}

		return nil
	}

	fmt.Println(i18n.G("All server addresses are unavailable"))
	fmt.Print(i18n.G("Please provide an alternate server address (empty to abort):") + " ")

	buf := bufio.NewReader(os.Stdin)
	line, _, err := buf.ReadLine()
	if err != nil {
		return err
	}

	if len(line) == 0 {
		return errors.New(i18n.G("Failed to add remote"))
	}

	err = c.addRemoteFromToken(string(line), server, token, rawToken.Fingerprint)
	if err != nil {
		return err
	}

	return nil
}

func (c *cmdRemoteAdd) addRemoteFromToken(addr string, server string, token string, fingerprint string) error {
	conf := c.global.conf

	var certificate *x509.Certificate
	var err error

	conf.Remotes[server] = config.Remote{
		Addr:      addr,
		Protocol:  c.flagProtocol,
		AuthType:  c.flagAuthType,
		KeepAlive: c.flagKeepAlive,
	}

	_, err = conf.GetInstanceServer(server)
	if err != nil {
		certificate, err = localtls.GetRemoteCertificate(addr, c.global.conf.UserAgent)
		if err != nil {
			return api.StatusErrorf(http.StatusServiceUnavailable, i18n.G("Unavailable remote server")+": %v", err)
		}

		certDigest := localtls.CertFingerprint(certificate)
		if fingerprint != certDigest {
			return fmt.Errorf(i18n.G("Certificate fingerprint mismatch between certificate token and server %q"), addr)
		}

		dnam := conf.ConfigPath("servercerts")
		err := os.MkdirAll(dnam, 0o750)
		if err != nil {
			return errors.New(i18n.G("Could not create server cert dir"))
		}

		certf := conf.ServerCertPath(server)

		certOut, err := os.Create(certf)
		if err != nil {
			return fmt.Errorf(i18n.G("Failed to create %q: %w"), certf, err)
		}

		err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
		if err != nil {
			return fmt.Errorf(i18n.G("Failed to write server cert file %q: %w"), certf, err)
		}

		err = certOut.Close()
		if err != nil {
			return fmt.Errorf(i18n.G("Failed to close server cert file %q: %w"), certf, err)
		}
	}

	d, err := conf.GetInstanceServer(server)
	if err != nil {
		return api.StatusErrorf(http.StatusServiceUnavailable, i18n.G("Unavailable remote server")+": %v", err)
	}

	req := api.CertificatesPost{
		TrustToken: token,
	}

	err = d.CreateCertificate(req)
	if err != nil {
		return fmt.Errorf(i18n.G("Failed to create certificate: %w"), err)
	}

	// Handle project.
	remote := conf.Remotes[server]
	project, err := c.findProject(d, c.flagProject)
	if err != nil {
		return fmt.Errorf(i18n.G("Failed to find project: %w"), err)
	}

	remote.Project = project
	conf.Remotes[server] = remote

	return conf.SaveConfig(c.global.confPath)
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteAdd) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 2)
	if exit {
		return err
	}

	// Determine server name and address
	server := args[0]
	addr := args[0]
	if len(args) > 1 {
		addr = args[1]
	}

	// Validate the server name.
	if strings.Contains(server, ":") {
		return errors.New(i18n.G("Remote names may not contain colons"))
	}

	// Check for existing remote
	remote, ok := conf.Remotes[server]
	if ok {
		return fmt.Errorf(i18n.G("Remote %s exists as <%s>"), server, remote.Addr)
	}

	// Parse the URL
	var rScheme string
	var rHost string
	var rPort string

	if c.flagProtocol == "" {
		c.flagProtocol = "incus"
	}

	// Initialize the remotes list if needed
	if conf.Remotes == nil {
		conf.Remotes = map[string]config.Remote{}
	}

	rawToken, err := localtls.CertificateTokenDecode(addr)
	if err == nil {
		return c.runToken(server, addr, rawToken)
	}

	// Complex remote URL parsing
	remoteURL, err := url.Parse(addr)
	if err != nil {
		remoteURL = &url.URL{Host: addr}
	}

	// Fast track image servers.
	if slices.Contains([]string{"oci", "simplestreams"}, c.flagProtocol) {
		if remoteURL.Scheme != "https" {
			return errors.New(i18n.G("Only https URLs are supported for oci and simplestreams"))
		}

		conf.Remotes[server] = config.Remote{
			Addr:       addr,
			Public:     true,
			Protocol:   c.flagProtocol,
			KeepAlive:  c.flagKeepAlive,
			CredHelper: c.flagCredHelper,
		}

		return conf.SaveConfig(c.global.confPath)
	} else if c.flagProtocol != "incus" {
		return fmt.Errorf(i18n.G("Invalid protocol: %s"), c.flagProtocol)
	}

	// Fix broken URL parser
	if !strings.Contains(addr, "://") && remoteURL.Scheme != "" && remoteURL.Scheme != "unix" && remoteURL.Host == "" {
		remoteURL.Host = addr
		remoteURL.Scheme = ""
	}

	if remoteURL.Scheme != "" {
		if remoteURL.Scheme != "unix" && remoteURL.Scheme != "https" {
			return fmt.Errorf(i18n.G("Invalid URL scheme \"%s\" in \"%s\""), remoteURL.Scheme, addr)
		}

		rScheme = remoteURL.Scheme
	} else if addr[0] == '/' {
		rScheme = "unix"
	} else {
		if !internalUtil.IsUnixSocket(addr) {
			rScheme = "https"
		} else {
			rScheme = "unix"
		}
	}

	if remoteURL.Host != "" {
		rHost = remoteURL.Host
	} else {
		rHost = addr
	}

	host, port, err := net.SplitHostPort(rHost)
	if err == nil {
		rHost = host
		rPort = port
	} else {
		rPort = fmt.Sprintf("%d", ports.HTTPSDefaultPort)
	}

	if rScheme == "unix" {
		rHost = strings.TrimPrefix(strings.TrimPrefix(addr, "unix:"), "//")
		rPort = ""
	}

	if strings.Contains(rHost, ":") && !strings.HasPrefix(rHost, "[") {
		rHost = fmt.Sprintf("[%s]", rHost)
	}

	if rPort != "" {
		addr = rScheme + "://" + rHost + ":" + rPort
	} else {
		addr = rScheme + "://" + rHost
	}

	// Finally, actually add the remote, almost...  If the remote is a private
	// HTTPS server then we need to ensure we have a client certificate before
	// adding the remote server.
	if rScheme != "unix" && !c.flagPublic && (c.flagAuthType == api.AuthenticationMethodTLS || c.flagAuthType == "") {
		if !conf.HasClientCertificate() {
			fmt.Fprint(os.Stderr, i18n.G("Generating a client certificate. This may take a minute...")+"\n")
			err = conf.GenerateClientCertificate()
			if err != nil {
				return err
			}
		}
	}

	conf.Remotes[server] = config.Remote{
		Addr:      addr,
		Protocol:  c.flagProtocol,
		AuthType:  c.flagAuthType,
		KeepAlive: c.flagKeepAlive,
	}

	// Attempt to connect
	var d incus.ImageServer
	if c.flagPublic {
		d, err = conf.GetImageServer(server)
	} else {
		d, err = conf.GetInstanceServer(server)
	}

	// Handle Unix socket connections
	if strings.HasPrefix(addr, "unix:") {
		if err != nil {
			return err
		}

		remote := conf.Remotes[server]
		remote.AuthType = api.AuthenticationMethodTLS

		// Handle project.
		project, err := c.findProject(d.(incus.InstanceServer), c.flagProject)
		if err != nil {
			return err
		}

		remote.Project = project

		conf.Remotes[server] = remote
		return conf.SaveConfig(c.global.confPath)
	}

	// Check if the system CA worked for the TLS connection
	var certificate *x509.Certificate
	if err != nil {
		// Failed to connect using the system CA, so retrieve the remote certificate
		certificate, err = localtls.GetRemoteCertificate(addr, c.global.conf.UserAgent)
		if err != nil {
			return err
		}
	}

	// Handle certificate prompt
	if certificate != nil {
		if !c.flagAcceptCert {
			digest := localtls.CertFingerprint(certificate)

			fmt.Printf(i18n.G("Certificate fingerprint: %s")+"\n", digest)
			fmt.Print(i18n.G("ok (y/n/[fingerprint])?") + " ")
			buf := bufio.NewReader(os.Stdin)
			line, _, err := buf.ReadLine()
			if err != nil {
				return err
			}

			if string(line) != digest {
				if len(line) < 1 || strings.ToLower(string(line[0])) == i18n.G("n") {
					return errors.New(i18n.G("Server certificate NACKed by user"))
				} else if strings.ToLower(string(line[0])) != i18n.G("y") {
					return errors.New(i18n.G("Please type 'y', 'n' or the fingerprint:"))
				}
			}
		}

		dnam := conf.ConfigPath("servercerts")
		err := os.MkdirAll(dnam, 0o750)
		if err != nil {
			return errors.New(i18n.G("Could not create server cert dir"))
		}

		certf := conf.ServerCertPath(server)
		certOut, err := os.Create(certf)
		if err != nil {
			return err
		}

		err = pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
		if err != nil {
			return fmt.Errorf(i18n.G("Could not write server cert file %q: %w"), certf, err)
		}

		err = certOut.Close()
		if err != nil {
			return fmt.Errorf(i18n.G("Could not close server cert file %q: %w"), certf, err)
		}

		// Setup a new connection, this time with the remote certificate
		if c.flagPublic {
			d, err = conf.GetImageServer(server)
		} else {
			d, err = conf.GetInstanceServer(server)
		}

		if err != nil {
			return err
		}
	}

	// Handle public remotes
	if c.flagPublic {
		conf.Remotes[server] = config.Remote{
			Addr:      addr,
			Public:    true,
			KeepAlive: c.flagKeepAlive,
		}

		return conf.SaveConfig(c.global.confPath)
	}

	// Get server information
	srv, _, err := d.(incus.InstanceServer).GetServer()
	if err != nil {
		return err
	}

	// If not specified, the preferred order of authentication is 1) OIDC 2) TLS.
	if c.flagAuthType == "" {
		if !srv.Public && slices.Contains(srv.AuthMethods, api.AuthenticationMethodOIDC) {
			c.flagAuthType = api.AuthenticationMethodOIDC
		} else {
			c.flagAuthType = api.AuthenticationMethodTLS
		}

		if slices.Contains([]string{api.AuthenticationMethodOIDC}, c.flagAuthType) {
			// Update the remote configuration
			remote := conf.Remotes[server]
			remote.AuthType = c.flagAuthType
			conf.Remotes[server] = remote

			// Re-setup the client
			d, err = conf.GetInstanceServer(server)
			if err != nil {
				return err
			}

			d.(incus.InstanceServer).RequireAuthenticated(false)

			srv, _, err = d.(incus.InstanceServer).GetServer()
			if err != nil {
				return err
			}
		} else {
			// Update the remote configuration
			remote := conf.Remotes[server]
			remote.AuthType = c.flagAuthType
			conf.Remotes[server] = remote
		}
	}

	if !srv.Public && !slices.Contains(srv.AuthMethods, c.flagAuthType) {
		return fmt.Errorf(i18n.G("Authentication type '%s' not supported by server"), c.flagAuthType)
	}

	// Detect public remotes
	if srv.Public {
		conf.Remotes[server] = config.Remote{Addr: addr, Public: true, KeepAlive: c.flagKeepAlive}
		return conf.SaveConfig(c.global.confPath)
	}

	// Check if additional authentication is required.
	if srv.Auth != "trusted" {
		if c.flagAuthType == api.AuthenticationMethodTLS {
			// Prompt for trust token
			if c.flagToken == "" {
				c.flagToken, err = c.global.asker.AskString(fmt.Sprintf(i18n.G("Trust token for %s: "), server), "", nil)
				if err != nil {
					return err
				}
			}

			// Add client certificate to trust store
			req := api.CertificatesPost{
				TrustToken: c.flagToken,
			}

			req.Type = api.CertificateTypeClient

			err = d.(incus.InstanceServer).CreateCertificate(req)
			if err != nil {
				return err
			}
		} else {
			d.(incus.InstanceServer).RequireAuthenticated(true)
		}

		// And check if trusted now
		srv, _, err = d.(incus.InstanceServer).GetServer()
		if err != nil {
			return err
		}

		if srv.Auth != "trusted" {
			return errors.New(i18n.G("Server doesn't trust us after authentication"))
		}

		if c.flagAuthType == api.AuthenticationMethodTLS {
			fmt.Println(i18n.G("Client certificate now trusted by server:"), server)
		}
	}

	// Handle project.
	remote = conf.Remotes[server]
	project, err := c.findProject(d.(incus.InstanceServer), c.flagProject)
	if err != nil {
		return err
	}

	remote.Project = project
	conf.Remotes[server] = remote

	return conf.SaveConfig(c.global.confPath)
}

// Generate certificate.
type cmdRemoteGenerateCertificate struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command generates the command definition.
func (c *cmdRemoteGenerateCertificate) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("generate-certificate")
	cmd.Short = i18n.G("Generate the client certificate")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Manually trigger the generation of a client certificate`))

	cmd.RunE = c.Run

	return cmd
}

// Run runs the actual command logic.
func (c *cmdRemoteGenerateCertificate) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 0, 0)
	if exit {
		return err
	}

	// Check if we already have a certificate.
	if conf.HasClientCertificate() {
		return errors.New(i18n.G("A client certificate is already present"))
	}

	// Generate the certificate.
	if !c.global.flagQuiet {
		fmt.Fprint(os.Stderr, i18n.G("Generating a client certificate. This may take a minute...")+"\n")
	}

	err = conf.GenerateClientCertificate()
	if err != nil {
		return err
	}

	return nil
}

// Get default.
type cmdRemoteGetDefault struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteGetDefault) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("get-default")
	cmd.Short = i18n.G("Show the default remote")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Show the default remote`))

	cmd.RunE = c.Run

	return cmd
}

// Get client certificate.
type cmdRemoteGetClientCertificate struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for get-client-certificate.
func (c *cmdRemoteGetClientCertificate) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("get-client-certificate")
	cmd.Short = i18n.G("Print the client certificate used by this Incus client")
	cmd.RunE = c.Run
	return cmd
}

// Run runs the actual command logic.
func (c *cmdRemoteGetClientCertificate) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 0, 0)
	if exit {
		return err
	}

	// Check if we need to generate a new certificate.
	if !conf.HasClientCertificate() {
		if !c.global.flagQuiet {
			fmt.Fprint(os.Stderr, i18n.G("Generating a client certificate. This may take a minute...")+"\n")
		}

		err = conf.GenerateClientCertificate()
		if err != nil {
			return err
		}
	}

	// Read the certificate.
	content, err := os.ReadFile(conf.ConfigPath("client.crt"))
	if err != nil {
		return fmt.Errorf("Failed to read certificate: %w", err)
	}

	fmt.Print(string(content))
	return nil
}

type cmdRemoteGetClientToken struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for get-client-token.
func (c *cmdRemoteGetClientToken) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("get-client-token <expiry>")
	cmd.Short = i18n.G("Generate a client token derived from the client certificate")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Generate a client trust token derived from the existing client certificate and private key.

This is useful for remote authentication workflows where a token is passed to another Incus server.`))
	cmd.RunE = c.Run
	return cmd
}

// Run runs the get-client-token logic.
func (c *cmdRemoteGetClientToken) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse the expiry.
	expiry, err := time.ParseDuration(args[0])
	if err != nil {
		return err
	}

	// Check if we need to generate a new certificate.
	if !conf.HasClientCertificate() {
		if !c.global.flagQuiet {
			fmt.Fprint(os.Stderr, i18n.G("Generating a client certificate. This may take a minute...")+"\n")
		}

		err = conf.GenerateClientCertificate()
		if err != nil {
			return err
		}
	}

	// Read the key pair.
	cert, err := os.ReadFile(conf.ConfigPath("client.crt"))
	if err != nil {
		return fmt.Errorf("Failed to read certificate: %w", err)
	}

	key, err := os.ReadFile(conf.ConfigPath("client.key"))
	if err != nil {
		return fmt.Errorf("Failed to read private key: %w", err)
	}

	keypair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		return err
	}

	// Use SHA-256 fingerprint of the first cert in the chain.
	fingerprint := sha256.Sum256(keypair.Certificate[0])
	subject := fmt.Sprintf("%x", fingerprint)

	now := time.Now()
	claims := jwt.RegisteredClaims{
		Subject:   subject,
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(expiry)),
	}

	// Trying signing with both ES384 and RS256.
	for _, alg := range []jwt.SigningMethod{jwt.SigningMethodES384, jwt.SigningMethodRS256} {
		token := jwt.NewWithClaims(alg, claims)
		tokenStr, err := token.SignedString(keypair.PrivateKey)
		if err == nil {
			fmt.Println(tokenStr)

			return nil
		}
	}

	return errors.New("Unable to sign JWT with available key algorithms")
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteGetDefault) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 0, 0)
	if exit {
		return err
	}

	// Show the default remote
	fmt.Println(conf.DefaultRemote)

	return nil
}

// List.
type cmdRemoteList struct {
	global *cmdGlobal
	remote *cmdRemote

	flagFormat  string
	flagColumns string
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteList) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("list")
	cmd.Aliases = []string{"ls"}
	cmd.Short = i18n.G("List the available remotes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`List the available remotes

Default column layout: nupaPsg

== Columns ==
The -c option takes a comma separated list of arguments that control
which instance attributes to output when displaying in table or csv
format.

Column arguments are either pre-defined shorthand chars (see below),
or (extended) config keys.

Commas between consecutive shorthand chars are optional.

Pre-defined column shorthand chars:
  n - Name
  u - URL
  p - Protocol
  a - Auth Type
  P - Public
  s - Static
  g - Global`))

	cmd.RunE = c.Run
	cmd.Flags().StringVarP(&c.flagFormat, "format", "f", c.global.defaultListFormat(), i18n.G(`Format (csv|json|table|yaml|compact|markdown), use suffix ",noheader" to disable headers and ",header" to enable it if missing, e.g. csv,header`)+"``")
	cmd.Flags().StringVarP(&c.flagColumns, "columns", "c", defaultRemoteColumns, i18n.G("Columns")+"``")

	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		return cli.ValidateFlagFormatForListOutput(cmd.Flag("format").Value.String())
	}

	return cmd
}

const defaultRemoteColumns = "nupaPsg"

func (c *cmdRemoteList) parseColumns() ([]remoteColumn, error) {
	columnsShorthandMap := map[rune]remoteColumn{
		'n': {i18n.G("NAME"), c.remoteNameColumnData},
		'u': {i18n.G("URL"), c.addrColumnData},
		'p': {i18n.G("PROTOCOL"), c.protocolColumnData},
		'a': {i18n.G("AUTH TYPE"), c.authTypeColumnData},
		'P': {i18n.G("PUBLIC"), c.publicColumnData},
		's': {i18n.G("STATIC"), c.staticColumnData},
		'g': {i18n.G("GLOBAL"), c.globalColumnData},
	}

	columnList := strings.Split(c.flagColumns, ",")
	columns := []remoteColumn{}

	for _, columnEntry := range columnList {
		if columnEntry == "" {
			return nil, fmt.Errorf(i18n.G("Empty column entry (redundant, leading or trailing command) in '%s'"), c.flagColumns)
		}

		for _, columnRune := range columnEntry {
			column, ok := columnsShorthandMap[columnRune]
			if !ok {
				return nil, fmt.Errorf(i18n.G("Unknown column shorthand char '%c' in '%s'"), columnRune, columnEntry)
			}

			columns = append(columns, column)
		}
	}

	return columns, nil
}

func (c *cmdRemoteList) remoteNameColumnData(name string, _ config.Remote) string {
	conf := c.global.conf

	strName := name
	if name == conf.DefaultRemote {
		strName = fmt.Sprintf("%s (%s)", name, i18n.G("current"))
	}

	return strName
}

func (c *cmdRemoteList) addrColumnData(_ string, rc config.Remote) string {
	return rc.Addr
}

func (c *cmdRemoteList) protocolColumnData(_ string, rc config.Remote) string {
	return rc.Protocol
}

func (c *cmdRemoteList) authTypeColumnData(_ string, rc config.Remote) string {
	if rc.AuthType == "" {
		if strings.HasPrefix(rc.Addr, "unix:") {
			rc.AuthType = "file access"
		} else if rc.Protocol != "incus" {
			rc.AuthType = "none"
		} else {
			rc.AuthType = api.AuthenticationMethodTLS
		}
	}

	return rc.AuthType
}

func (c *cmdRemoteList) publicColumnData(_ string, rc config.Remote) string {
	strPublic := i18n.G("NO")
	if rc.Public {
		strPublic = i18n.G("YES")
	}

	return strPublic
}

func (c *cmdRemoteList) staticColumnData(_ string, rc config.Remote) string {
	strStatic := i18n.G("NO")
	if rc.Static {
		strStatic = i18n.G("YES")
	}

	return strStatic
}

func (c *cmdRemoteList) globalColumnData(_ string, rc config.Remote) string {
	strGlobal := i18n.G("NO")
	if rc.Global {
		strGlobal = i18n.G("YES")
	}

	return strGlobal
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteList) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 0, 0)
	if exit {
		return err
	}

	columns, err := c.parseColumns()
	if err != nil {
		return err
	}

	// List the remotes
	data := [][]string{}
	for name, rc := range conf.Remotes {
		line := []string{}
		for _, column := range columns {
			line = append(line, column.Data(name, rc))
		}

		data = append(data, line)
	}

	sort.Sort(cli.SortColumnsNaturally(data))

	header := []string{}
	for _, column := range columns {
		header = append(header, column.Name)
	}

	return cli.RenderTable(os.Stdout, c.flagFormat, header, data, conf.Remotes)
}

// Rename.
type cmdRemoteRename struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteRename) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("rename", i18n.G("<remote> <new-name>"))
	cmd.Aliases = []string{"mv"}
	cmd.Short = i18n.G("Rename remotes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Rename remotes`))

	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpRemoteNames()
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteRename) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Rename the remote
	rc, ok := conf.Remotes[args[0]]
	if !ok {
		return fmt.Errorf(i18n.G("Remote %s doesn't exist"), args[0])
	}

	if rc.Static {
		return fmt.Errorf(i18n.G("Remote %s is static and cannot be modified"), args[0])
	}

	_, ok = conf.Remotes[args[1]]
	if ok {
		return fmt.Errorf(i18n.G("Remote %s already exists"), args[1])
	}

	// Rename the certificate file
	oldPath := conf.ServerCertPath(args[0])
	newPath := conf.ServerCertPath(args[1])
	if util.PathExists(oldPath) {
		if conf.Remotes[args[0]].Global {
			err := conf.CopyGlobalCert(args[0], args[1])
			if err != nil {
				return err
			}
		} else {
			err := os.Rename(oldPath, newPath)
			if err != nil {
				return err
			}
		}
	}

	rc.Global = false
	conf.Remotes[args[1]] = rc
	delete(conf.Remotes, args[0])

	if conf.DefaultRemote == args[0] {
		conf.DefaultRemote = args[1]
	}

	return conf.SaveConfig(c.global.confPath)
}

// Remove.
type cmdRemoteRemove struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteRemove) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("remove", i18n.G("<remote>"))
	cmd.Aliases = []string{"delete", "rm"}
	cmd.Short = i18n.G("Remove remotes")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Remove remotes`))

	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpRemoteNames()
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteRemove) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Remove the remote
	rc, ok := conf.Remotes[args[0]]
	if !ok {
		return fmt.Errorf(i18n.G("Remote %s doesn't exist"), args[0])
	}

	if rc.Static {
		return fmt.Errorf(i18n.G("Remote %s is static and cannot be modified"), args[0])
	}

	if rc.Global {
		return fmt.Errorf(i18n.G("Remote %s is global and cannot be removed"), args[0])
	}

	if conf.DefaultRemote == args[0] {
		return errors.New(i18n.G("Can't remove the default remote"))
	}

	delete(conf.Remotes, args[0])

	_ = os.Remove(conf.ServerCertPath(args[0]))
	_ = os.Remove(conf.CookiesPath(args[0]))
	_ = os.Remove(conf.OIDCTokenPath(args[0]))

	return conf.SaveConfig(c.global.confPath)
}

// Set default.
type cmdRemoteSwitch struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteSwitch) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Aliases = []string{"set-default"}
	cmd.Use = usage("switch", i18n.G("<remote>"))
	cmd.Short = i18n.G("Switch the default remote")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Switch the default remote`))

	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpRemoteNames()
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteSwitch) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Set the default remote
	_, ok := conf.Remotes[args[0]]
	if !ok {
		return fmt.Errorf(i18n.G("Remote %s doesn't exist"), args[0])
	}

	conf.DefaultRemote = args[0]

	return conf.SaveConfig(c.global.confPath)
}

// Set URL.
type cmdRemoteSetURL struct {
	global *cmdGlobal
	remote *cmdRemote
}

// Command returns a cobra.Command for use with (*cobra.Command).AddCommand.
func (c *cmdRemoteSetURL) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("set-url", i18n.G("<remote> <URL>"))
	cmd.Short = i18n.G("Set the URL for the remote")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Set the URL for the remote`))

	cmd.RunE = c.Run

	cmd.ValidArgsFunction = func(_ *cobra.Command, args []string, _ string) ([]string, cobra.ShellCompDirective) {
		if len(args) == 0 {
			return c.global.cmpRemoteNames()
		}

		return nil, cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

// Run is used in the RunE field of the cobra.Command returned by Command.
func (c *cmdRemoteSetURL) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	// Quick checks.
	exit, err := c.global.checkArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Set the URL
	rc, ok := conf.Remotes[args[0]]
	if !ok {
		return fmt.Errorf(i18n.G("Remote %s doesn't exist"), args[0])
	}

	if rc.Static {
		return fmt.Errorf(i18n.G("Remote %s is static and cannot be modified"), args[0])
	}

	remote := conf.Remotes[args[0]]
	if remote.Global {
		err := conf.CopyGlobalCert(args[0], args[0])
		if err != nil {
			return err
		}

		remote.Global = false
		conf.Remotes[args[0]] = remote
	}

	remote.Addr = args[1]
	conf.Remotes[args[0]] = remote

	return conf.SaveConfig(c.global.confPath)
}
