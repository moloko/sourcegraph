package gitserver

import (
	"context"
	"encoding/hex"
	"io/fs"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/sourcegraph/sourcegraph/internal/api"
	"github.com/sourcegraph/sourcegraph/internal/authz"
	"github.com/sourcegraph/sourcegraph/internal/database"
	"github.com/sourcegraph/sourcegraph/internal/types"
	"github.com/sourcegraph/sourcegraph/lib/errors"
)

type SQLRepo struct {
	db database.DB
}

func (r *SQLRepo) ReadDir(ctx context.Context, checker authz.SubRepoPermissionChecker, repo api.RepoName, commit api.CommitID, path string, recurse bool) ([]fs.FileInfo, error) {
	return nil, nil
}

func (r *SQLRepo) Clone(ctx context.Context, repoID api.RepoID, url string, branch string) error {
	// 1. Clone git repo with in-memory storage
	s := memory.NewStorage()
	gitRepo, err := gogit.Clone(s, nil, &gogit.CloneOptions{
		URL: url,
	})
	if err != nil {
		return err
	}
	// 2. Traverse all dependencies from the branch up to the root
	//    to create a children (reverse-parent) relationship and the list of roots.
	// TODO: Consider inserting versions with parent and child relationships into the database.
	children := map[plumbing.Hash][]plumbing.Hash{} // pointing at childred commits given a parent commit
	var roots []plumbing.Hash                       // commits with no parents
	gitBranch, err := gitRepo.Branch(branch)
	if err != nil {
		return err
	}
	gitBranchRef, err := storer.ResolveReference(s, gitBranch.Merge)
	if err != nil {
		return err
	}
	iter, err := gitRepo.Log(&gogit.LogOptions{From: gitBranchRef.Hash()})
	if err != nil {
		return err
	}
	err = iter.ForEach(func(c *object.Commit) error {
		for _, h := range c.ParentHashes {
			ch := children[h]
			ch = append(ch, c.Hash)
			children[h] = ch
		}
		if len(c.ParentHashes) == 0 {
			roots = append(roots, c.Hash)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(roots) == 0 {
		return errors.New("want at least one root commit")
	}
	// 3. Going from roots to children in a manner such that
	//    parents are already present in the files relation
	//    when children are being inserted, fill in the files relation.
	toBeProcessed := new(bag)
	for _, r := range roots {
		c, err := gitRepo.CommitObject(r)
		if err != nil {
			return err
		}
		toBeProcessed.add(c)
	}
	processed := map[plumbing.Hash]bool{}
	paths := pathCoverage{topIndices: map[int]int{}}
	for toBeProcessed.notEmpty() {
		c := toBeProcessed.pop()
		var parents []*types.RepoVersion
		for _, h := range c.ParentHashes {
			p, err := r.db.RepoVersions().Lookup(ctx, repoID, hex.EncodeToString(h[:]))
			if err != nil {
				return err
			}
			if p == nil {
				return errors.Errorf("expected to find %q in database", hex.EncodeToString(h[:]))
			}
			parents = append(parents, p)
		}
		reachability := aggregateReachability(parents)
		coverage := paths.TakeIndex(reachability)
		reachability[coverage.PathColor] = coverage.PathIndex // can reach self
		// TODO compute path color and index based on intermediate path storage.
		// TODO compute reachability based on parents already present in the database.
		v := types.RepoVersion{
			RepoID:       repoID,
			ExternalID:   hex.EncodeToString(c.Hash[:]),
			PathCoverage: coverage,
			Reachability: reachability,
		}
		_, err = r.db.RepoVersions().CreateIfNotExists(ctx, v)
		if err != nil {
			return err
		}
		// TODO here process the files for the commit - got parents
		// so we can compute file changes as well.
		processed[c.Hash] = true
		// Check if any child of this commit has all its parents
		// processed. If so, add it to the bag
		for _, ch := range children[c.Hash] {
			if processed[ch] {
				continue
			}
			chCommit, err := gitRepo.CommitObject(ch)
			if err != nil {
				return err
			}
			chParentsProcessed := true
			for _, ph := range chCommit.ParentHashes {
				if !processed[ph] {
					chParentsProcessed = false
					break
				}
			}
			if chParentsProcessed {
				toBeProcessed.add(chCommit)
			}
		}
	}
	return nil
}

// bag of commits is used to keep track of commits
// that are next to be processed.
type bag struct {
	stack []*object.Commit
}

func (b *bag) notEmpty() bool {
	return len(b.stack) > 0
}

func (b *bag) pop() *object.Commit {
	c := b.stack[len(b.stack)-1]
	b.stack = b.stack[:len(b.stack)-1]
	return c
}

func (b *bag) add(c *object.Commit) {
	b.stack = append(b.stack, c)
}

// computing reachability
//
// just take the max index for every reachable path color
//
// for the index of considered commit, try to extend the lowest color
// path. That is if the reachable index for a path is the highest index
// for a path, then that can be extended - that is this commit can get
// the same color and index+1.

func aggregateReachability(parents []*types.RepoVersion) map[int]int {
	r := map[int]int{}
	for _, p := range parents {
		for c, i := range p.Reachability {
			j := r[c]
			if i > j {
				r[c] = i
			}
		}
	}
	return r
}

type pathCoverage struct {
	topIndices       map[int]int
	highestUsedColor int
}

const maxUint = ^uint(0)
const maxInt = int(maxUint >> 1)

func (p *pathCoverage) TakeIndex(reachability map[int]int) types.RepoVersionPathCoverage {
	lowestUsableColor := maxInt
	for c, i := range reachability {
		if p.topIndices[c] == i { // can use C
			// pick the lowest color - which is somewhat of an arbitrary strategy,
			// but let's go with that for now
			if c < lowestUsableColor {
				lowestUsableColor = c
			}
		}
	}
	// No path can be extended, pick a new color.
	if lowestUsableColor == maxInt {
		lowestUsableColor = p.highestUsedColor + 1
	}
	// if new color was picked this will be 0+1 = 1, so the lowest index that can be used.
	// as we start counting path index from 1
	index := p.topIndices[lowestUsableColor] + 1
	// Update the state of path coverage
	p.topIndices[lowestUsableColor] = index
	if p.highestUsedColor < lowestUsableColor {
		p.highestUsedColor = lowestUsableColor
	}
	return types.RepoVersionPathCoverage{
		PathColor: lowestUsableColor,
		PathIndex: index,
	}
}
