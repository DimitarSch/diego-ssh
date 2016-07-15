package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
	"syscall"

	"code.cloudfoundry.org/cflager"
	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/diego-ssh/authenticators"
	"code.cloudfoundry.org/diego-ssh/daemon"
	"code.cloudfoundry.org/diego-ssh/keys"
	"code.cloudfoundry.org/lager"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"
	"golang.org/x/crypto/ssh"
)

var address = flag.String(
	"address",
	"127.0.0.1:2222",
	"listen address for ssh daemon",
)

var hostKey = flag.String(
	"hostKey",
	"",
	"PEM encoded RSA host key",
)

var authorizedKey = flag.String(
	"authorizedKey",
	"",
	"Public key in the OpenSSH authorized_keys format",
)

var allowUnauthenticatedClients = flag.Bool(
	"allowUnauthenticatedClients",
	false,
	"Allow access to unauthenticated clients",
)

var inheritDaemonEnv = flag.Bool(
	"inheritDaemonEnv",
	false,
	"Inherit daemon's environment",
)

var allowedCiphers = flag.String(
	"allowedCiphers",
	"",
	"Limit cipher algorithms to those provided (comma separated)",
)

var allowedMACs = flag.String(
	"allowedMACs",
	"",
	"Limit MAC algorithms to those provided (comma separated)",
)

var allowedKeyExchanges = flag.String(
	"allowedKeyExchanges",
	"",
	"Limit key exchanges algorithms to those provided (comma separated)",
)

var hostKeyPEM string
var authorizedKeyValue string

func main() {
	debugserver.AddFlags(flag.CommandLine)
	cflager.AddFlags(flag.CommandLine)
	flag.Parse()
	exec := false

	envHostKey := os.Getenv("SSHD_HOSTKEY")
	if envHostKey != "" {
		hostKeyPEM = envHostKey
		os.Unsetenv("SSHD_HOSTKEY")
	} else {
		hostKeyPEM = *hostKey
		exec = true
	}
	envAuthKey := os.Getenv("SSHD_AUTHKEY")
	if envAuthKey != "" {
		authorizedKeyValue = envAuthKey
		os.Unsetenv("SSHD_AUTHKEY")
	} else {
		authorizedKeyValue = *authorizedKey
		exec = true
	}

	logger, reconfigurableSink := cflager.New("sshd")

	if exec {
		os.Setenv("SSHD_HOSTKEY", hostKeyPEM)
		os.Setenv("SSHD_AUTHKEY", authorizedKeyValue)

		err := syscall.Exec(os.Args[0], []string{
			fmt.Sprintf("--allowedKeyExchanges=%s", *allowedKeyExchanges),
			fmt.Sprintf("--address=%s", *address),
			fmt.Sprintf("--allowUnauthenticatedClients=%t", *allowUnauthenticatedClients),
			fmt.Sprintf("--inheritDaemonEnv=%t", *inheritDaemonEnv),
			fmt.Sprintf("--allowedCiphers=%s", *allowedCiphers),
			fmt.Sprintf("--allowedMACs=%s", *allowedMACs),
		}, os.Environ())
		if err != nil {
			logger.Error("failed-exec", err)
			os.Exit(1)
		}
	}

	serverConfig, err := configure(logger)
	if err != nil {
		logger.Error("configure-failed", err)
		os.Exit(1)
	}

	sshDaemon := daemon.New(logger, serverConfig, nil, newChannelHandlers())
	server, err := createServer(logger, *address, sshDaemon)

	members := grouper.Members{
		{"sshd", server},
	}

	if dbgAddr := debugserver.DebugAddress(flag.CommandLine); dbgAddr != "" {
		members = append(grouper.Members{
			{"debug-server", debugserver.Runner(dbgAddr, reconfigurableSink)},
		}, members...)
	}

	group := grouper.NewOrdered(os.Interrupt, members)
	monitor := ifrit.Invoke(sigmon.New(group))

	logger.Info("started")

	err = <-monitor.Wait()
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
	os.Exit(0)
}

func getDaemonEnvironment() map[string]string {
	daemonEnv := map[string]string{}

	if *inheritDaemonEnv {
		envs := os.Environ()
		for _, env := range envs {
			nvp := strings.SplitN(env, "=", 2)
			if len(nvp) == 2 && nvp[0] != "PATH" {
				daemonEnv[nvp[0]] = nvp[1]
			}
		}
	}
	return daemonEnv
}

func configure(logger lager.Logger) (*ssh.ServerConfig, error) {
	errorStrings := []string{}
	sshConfig := &ssh.ServerConfig{}

	key, err := acquireHostKey(logger)
	if err != nil {
		logger.Error("failed-to-acquire-host-key", err)
		errorStrings = append(errorStrings, err.Error())
	}

	sshConfig.AddHostKey(key)
	sshConfig.NoClientAuth = *allowUnauthenticatedClients

	if authorizedKeyValue == "" && !*allowUnauthenticatedClients {
		logger.Error("authorized-key-required", nil)
		errorStrings = append(errorStrings, "Public user key is required")
	}

	if authorizedKeyValue != "" {
		decodedPublicKey, err := decodeAuthorizedKey(logger)
		if err == nil {
			authenticator := authenticators.NewPublicKeyAuthenticator(decodedPublicKey)
			sshConfig.PublicKeyCallback = authenticator.Authenticate
		} else {
			errorStrings = append(errorStrings, err.Error())
		}
	}

	if *allowedCiphers != "" {
		sshConfig.Config.Ciphers = strings.Split(*allowedCiphers, ",")
	}
	if *allowedMACs != "" {
		sshConfig.Config.MACs = strings.Split(*allowedMACs, ",")
	}
	if *allowedKeyExchanges != "" {
		sshConfig.Config.KeyExchanges = strings.Split(*allowedKeyExchanges, ",")
	}

	err = nil
	if len(errorStrings) > 0 {
		err = errors.New(strings.Join(errorStrings, ", "))
	}

	return sshConfig, err
}

func decodeAuthorizedKey(logger lager.Logger) (ssh.PublicKey, error) {
	publicKey, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKeyValue))
	return publicKey, err
}

func acquireHostKey(logger lager.Logger) (ssh.Signer, error) {
	var encoded []byte
	if hostKeyPEM == "" {
		hostKeyPair, err := keys.RSAKeyPairFactory.NewKeyPair(1024)

		if err != nil {
			logger.Error("failed-to-generate-host-key", err)
			return nil, err
		}
		encoded = []byte(hostKeyPair.PEMEncodedPrivateKey())
	} else {
		encoded = []byte(hostKeyPEM)
	}

	key, err := ssh.ParsePrivateKey(encoded)
	if err != nil {
		logger.Error("failed-to-parse-host-key", err)
		return nil, err
	}
	return key, nil
}
