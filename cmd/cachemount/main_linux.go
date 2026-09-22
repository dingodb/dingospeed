// cachemount presents existing DingCache files as a read-only model directory.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"github.com/hanwen/go-fuse/v2/fuse"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"dingospeed/pkg/cachemount"
	"dingospeed/pkg/hfprojection"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}
func run() error {
	treeRoot := flag.String("tree-root", "", "discover HF and hosted models under this local cache root")
	treeConfig := flag.String("tree-config", "", "JSON array of selected cache_root/namespace/repo/revision objects")
	reportPath := flag.String("report", "", "write pinned version/file mapping JSON outside the mount")
	manifest := flag.String("manifest", "", "JSON object mapping model-relative paths to absolute OLAH cache paths")
	root := flag.String("cache-root", "", "existing HF cache root (alternative to -manifest)")
	repo := flag.String("repo", "", "HF repository, e.g. owner/model")
	revision := flag.String("revision", "main", "cached revision to pin")
	flag.Parse()
	if *treeRoot != "" || *treeConfig != "" {
		if flag.NArg() != 1 || (*treeRoot != "" && *treeConfig != "") || *manifest != "" || *root != "" {
			return fmt.Errorf("use exactly one of -tree-root or -tree-config and one mountpoint")
		}
		var selections []cachemount.Selection
		if *treeRoot != "" {
			var err error
			selections, err = cachemount.Discover(*treeRoot)
			if err != nil {
				return err
			}
		} else {
			b, err := os.ReadFile(*treeConfig)
			if err != nil {
				return err
			}
			if err = json.Unmarshal(b, &selections); err != nil {
				return err
			}
		}
		if len(selections) == 0 {
			return fmt.Errorf("no model versions found or selected")
		}
		mountpoint, err := filepath.Abs(flag.Arg(0))
		if err != nil {
			return err
		}
		if *reportPath != "" {
			reportAbs, err := filepath.Abs(*reportPath)
			if err != nil {
				return err
			}
			rel, err := filepath.Rel(mountpoint, reportAbs)
			if err != nil {
				return err
			}
			if rel == "." || (!filepath.IsAbs(rel) && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
				return fmt.Errorf("report must be outside mountpoint")
			}
		}
		snapshot, versions, err := cachemount.PrepareTree(selections, mountpoint)
		if err != nil {
			return err
		}
		defer snapshot.Close()
		server, err := cachemount.Mount(snapshot, mountpoint)
		if err != nil {
			return err
		}
		b, err := json.MarshalIndent(versions, "", "  ")
		if err != nil {
			server.Unmount()
			return err
		}
		if *reportPath != "" {
			f, err := os.OpenFile(*reportPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
			if err != nil {
				server.Unmount()
				return err
			}
			_, err = f.Write(b)
			closeErr := f.Close()
			if err != nil {
				server.Unmount()
				return err
			}
			if closeErr != nil {
				server.Unmount()
				return closeErr
			}
		}
		for _, v := range versions {
			log.Printf("%s %s (%d files) %s", v.Status, v.SourceFilePath, len(v.Files), v.Error)
		}
		return serve(server)
	}
	if flag.NArg() != 1 || (*manifest == "") == (*root == "") {
		return fmt.Errorf("usage: cachemount (-manifest files.json | -cache-root ROOT -repo OWNER/MODEL [-revision REV]) MOUNTPOINT")
	}
	sources := map[string]string{}
	expected := map[string]int64{}
	if *manifest != "" {
		b, err := os.ReadFile(*manifest)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(b, &sources); err != nil {
			return err
		}
	} else {
		m, paths, err := (hfprojection.Reader{Root: *root}).MountSources("models", *repo, *revision)
		if err != nil {
			return err
		}
		sources = paths
		for _, f := range m.Files {
			expected[f.Path] = f.Size
		}
		log.Printf("pinning HF commit %s", m.Commit)
	}
	for name, p := range sources {
		if !filepath.IsAbs(p) {
			return fmt.Errorf("%s: backing path must be absolute", name)
		}
	}
	snapshot, err := cachemount.Pin(sources)
	if err != nil {
		return err
	}
	defer snapshot.Close()
	for name, size := range expected {
		if size >= 0 && snapshot.Files[name].Size != size {
			return fmt.Errorf("%s: cache size differs from repository manifest", name)
		}
	}
	server, err := cachemount.Mount(snapshot, flag.Arg(0))
	if err != nil {
		return err
	}
	log.Printf("mounted %d files read-only at %s; no payload copied", len(sources), flag.Arg(0))
	return serve(server)
}

func serve(server *fuse.Server) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sig)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			select {
			case <-sig:
				if err := server.Unmount(); err != nil {
					log.Printf("unmount failed (stop readers and run fusermount3 -u): %v", err)
				}
			case <-done:
				return
			}
		}
	}()
	server.Wait()
	return nil
}
