package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/rborso/umkim-thin/internal/app"
	"github.com/rborso/umkim-thin/internal/cert"
)

const certUsage = `cert — manual TLS certificates

Usage:
  umkim-thin cert add --name N --cert FILE --key FILE [--hosts a,b] [--default] [--namespace NS]
  umkim-thin cert list
  umkim-thin cert default --name N
  umkim-thin cert remove --name N [--namespace NS]

A per-route certificate lives in the app namespace and is selected with
--certificate secret:<name>. A --default wildcard lives in Traefik's namespace
and becomes the default TLS store.
`

func certCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(certUsage)
		return nil
	}
	cfg := cert.Config{}
	ac := app.ConfigFromEnv()
	cfg = cert.Config{
		Kubectl: ac.Kubectl, Kubeconfig: ac.Kubeconfig, TraefikNS: ac.TraefikNS,
		Helm: ac.Helm, IdentityNS: ac.IdentityNS, IdentityChart: ac.IdentityChart,
		IdentityValues: ac.IdentityValues, IdentityApps: ac.IdentityApps,
		IdentityCerts: filepath.Join(ac.StateDir, "identity-certs.yml"),
	}
	store := cert.New(cfg)
	switch args[0] {
	case "add":
		fs := flag.NewFlagSet("cert add", flag.ContinueOnError)
		name := fs.String("name", "", "certificate name (the Secret name)")
		certFile := fs.String("cert", "", "PEM certificate file")
		keyFile := fs.String("key", "", "PEM private key file")
		hosts := fs.String("hosts", "", "comma-separated DNS names (default: certificate SANs)")
		setDefault := fs.Bool("default", false, "make this the Traefik default certificate (must be in Traefik's namespace)")
		namespace := fs.String("namespace", "", "Secret namespace (default: Traefik's namespace)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" || *certFile == "" || *keyFile == "" {
			return fmt.Errorf("cert add needs --name, --cert and --key")
		}
		certPEM, err := os.ReadFile(*certFile)
		if err != nil {
			return err
		}
		keyPEM, err := os.ReadFile(*keyFile)
		if err != nil {
			return err
		}
		var list []string
		for _, h := range strings.Split(*hosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				list = append(list, h)
			}
		}
		info, err := store.Add(*name, *namespace, certPEM, keyPEM, list, *setDefault)
		if err != nil {
			return err
		}
		fmt.Printf("certificate %s/%s\n  hosts: %s\n  expires: %s\n", info.Namespace, info.Name, strings.Join(info.Hosts, ", "), info.NotAfter.Format("2006-01-02"))
		if info.Default {
			fmt.Println("  default TLS store updated")
		}
		return nil
	case "list":
		infos, err := store.List()
		if err != nil {
			return err
		}
		for _, i := range infos {
			mark := ""
			if i.Default {
				mark = " [default]"
			}
			fmt.Printf("%-24s %-14s %-10s %s%s\n", i.Name, i.Namespace, i.NotAfter.Format("2006-01-02"), strings.Join(i.Hosts, ","), mark)
		}
		return nil
	case "default":
		fs := flag.NewFlagSet("cert default", flag.ContinueOnError)
		name := fs.String("name", "", "certificate name (in Traefik's namespace)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("cert default needs --name")
		}
		return store.SetDefault(*name)
	case "remove":
		fs := flag.NewFlagSet("cert remove", flag.ContinueOnError)
		name := fs.String("name", "", "certificate name")
		namespace := fs.String("namespace", "", "Secret namespace (default: Traefik's namespace)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *name == "" {
			return fmt.Errorf("cert remove needs --name")
		}
		return store.Remove(*name, *namespace)
	default:
		return fmt.Errorf("unknown cert subcommand %q", args[0])
	}
}
