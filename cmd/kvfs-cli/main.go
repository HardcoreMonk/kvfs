// Command kvfs-cli is the developer CLI:
//
//	kvfs-cli sign --method PUT --path /v1/o/bucket/key --ttl 1h
//	kvfs-cli inspect [--db ./edge-data/edge.db] [--object bucket/key]
//
// The secret for sign/verify is read from EDGE_URLKEY_SECRET (shared with kvfs-edge).
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/HardcoreMonk/kvfs/internal/store"
	"github.com/HardcoreMonk/kvfs/internal/urlkey"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "sign":
		cmdSign(os.Args[2:])
	case "inspect":
		cmdInspect(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, `kvfs-cli — kvfs developer tool

Subcommands:
  sign      Generate an HMAC-signed presigned URL
  inspect   Dump bbolt metadata store contents

Run 'kvfs-cli <subcommand> -h' for subcommand help.`)
}

// ---- sign ----

func cmdSign(args []string) {
	fs := flag.NewFlagSet("sign", flag.ExitOnError)
	method := fs.String("method", "GET", "HTTP method")
	path := fs.String("path", "", "request path (e.g. /v1/o/bucket/key)")
	ttl := fs.Duration("ttl", 1*time.Hour, "signature TTL")
	base := fs.String("base", "", "optional base URL prepended to the result (e.g. http://localhost:8000)")
	fs.Parse(args)

	if *path == "" {
		fmt.Fprintln(os.Stderr, "sign: -path required")
		os.Exit(2)
	}
	secret := os.Getenv("EDGE_URLKEY_SECRET")
	if secret == "" {
		fmt.Fprintln(os.Stderr, "sign: EDGE_URLKEY_SECRET env required")
		os.Exit(2)
	}

	signer, err := urlkey.NewSigner([]byte(secret))
	if err != nil {
		fail(err)
	}
	rel := signer.BuildURL(strings.ToUpper(*method), *path, *ttl)
	if *base != "" {
		fmt.Println(strings.TrimRight(*base, "/") + rel)
	} else {
		fmt.Println(rel)
	}
}

// ---- inspect ----

func cmdInspect(args []string) {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	dbPath := fs.String("db", "./edge-data/edge.db", "bbolt file path")
	oneObj := fs.String("object", "", "limit to single object key (format: bucket/key)")
	fs.Parse(args)

	ms, err := store.Open(*dbPath)
	if err != nil {
		fail(err)
	}
	defer ms.Close()

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")

	if *oneObj != "" {
		parts := strings.SplitN(*oneObj, "/", 2)
		if len(parts) != 2 {
			fail(fmt.Errorf("-object must be bucket/key"))
		}
		obj, err := ms.GetObject(parts[0], parts[1])
		if err != nil {
			fail(err)
		}
		_ = enc.Encode(obj)
		return
	}

	objs, err := ms.ListObjects()
	if err != nil {
		fail(err)
	}
	dns, _ := ms.ListDNs()
	_ = enc.Encode(map[string]any{
		"objects": objs,
		"dns":     dns,
	})
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "kvfs-cli:", err)
	os.Exit(1)
}
