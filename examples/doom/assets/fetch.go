// Package assets downloads the Doom inputs the example runs: Doom compiled to
// LLVM IR and the shareware DOOM1.WAD, from Turso's turso-vdbe-doom-example
// repository at a pinned commit. The checkout is cached, so later runs work
// offline.
package assets

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

const (
	repoURL = "https://github.com/tursodatabase/turso-vdbe-doom-example"
	commit  = "52be17f94914a90f2e6af8e2ab296f478534e679"
)

// Fetch makes sure the pinned checkout is in the cache and returns the
// directory of .ll modules and the path of DOOM1.WAD.
func Fetch() (llDir, wad string, err error) {
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", "", err
	}
	dir := filepath.Join(cache, "musql-doom", commit)
	llDir, wad = filepath.Join(dir, "doom", "ll"), filepath.Join(dir, "doom", "DOOM1.WAD")
	if _, err := os.Stat(wad); err == nil {
		return llDir, wad, nil
	}
	fmt.Fprintf(os.Stderr, "fetching Doom from %s ...\n", repoURL)
	if err := checkout(dir); err != nil {
		os.RemoveAll(dir)
		return "", "", err
	}
	return llDir, wad, nil
}

// checkout fetches exactly the pinned commit, depth 1, and checks out only
// its doom/ directory.
func checkout(dir string) error {
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		return err
	}
	remote, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{repoURL}})
	if err != nil {
		return err
	}
	err = remote.Fetch(&git.FetchOptions{
		RefSpecs: []config.RefSpec{config.RefSpec(commit + ":refs/heads/doom")},
		Depth:    1,
	})
	if err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	return wt.Checkout(&git.CheckoutOptions{
		Hash:                      plumbing.NewHash(commit),
		SparseCheckoutDirectories: []string{"doom"},
	})
}
