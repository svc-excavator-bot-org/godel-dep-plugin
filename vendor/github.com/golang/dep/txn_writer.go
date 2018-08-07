// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package dep

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"

	"github.com/golang/dep/gps"
	"github.com/golang/dep/gps/verify"
	"github.com/golang/dep/internal/fs"
	"github.com/pkg/errors"
)

const (
	// Helper consts for common diff-checking patterns.
	anyExceptHash verify.DeltaDimension = verify.AnyChanged & ^verify.HashVersionChanged & ^verify.HashChanged
)

// Example string to be written to the manifest file
// if no dependencies are found in the project
// during `dep init`
var exampleTOML = []byte(`# Gopkg.toml example
#
# Refer to https://golang.github.io/dep/docs/Gopkg.toml.html
# for detailed Gopkg.toml documentation.
#
# required = ["github.com/user/thing/cmd/thing"]
# ignored = ["github.com/user/project/pkgX", "bitbucket.org/user/project/pkgA/pkgY"]
#
# [[constraint]]
#   name = "github.com/user/project"
#   version = "1.0.0"
#
# [[constraint]]
#   name = "github.com/user/project2"
#   branch = "dev"
#   source = "github.com/myfork/project2"
#
# [[override]]
#   name = "github.com/x/y"
#   version = "2.4.0"
#
# [prune]
#   non-go = false
#   go-tests = true
#   unused-packages = true

`)

// String added on top of lock file
var lockFileComment = []byte(`# This file is autogenerated, do not edit; changes may be undone by the next 'dep ensure'.

`)

// SafeWriter transactionalizes writes of manifest, lock, and vendor dir, both
// individually and in any combination, into a pseudo-atomic action with
// transactional rollback.
//
// It is not impervious to errors (writing to disk is hard), but it should
// guard against non-arcane failure conditions.
type SafeWriter struct {
	Manifest     *Manifest
	lock         *Lock
	lockDiff     verify.LockDelta
	writeVendor  bool
	writeLock    bool
	pruneOptions gps.CascadingPruneOptions
}

// NewSafeWriter sets up a SafeWriter to write a set of manifest, lock, and
// vendor tree.
//
// - If manifest is provided, it will be written to the standard manifest file
// name beneath root.
//
// - If newLock is provided, it will be written to the standard lock file
// name beneath root.
//
// - If vendor is VendorAlways, or is VendorOnChanged and the locks are different,
// the vendor directory will be written beneath root based on newLock.
//
// - If oldLock is provided without newLock, error.
//
// - If vendor is VendorAlways without a newLock, error.
func NewSafeWriter(manifest *Manifest, oldLock, newLock *Lock, vendor VendorBehavior, prune gps.CascadingPruneOptions, status map[string]verify.VendorStatus) (*SafeWriter, error) {
	sw := &SafeWriter{
		Manifest:     manifest,
		lock:         newLock,
		pruneOptions: prune,
	}

	if oldLock != nil {
		if newLock == nil {
			return nil, errors.New("must provide newLock when oldLock is specified")
		}

		sw.lockDiff = verify.DiffLocks(oldLock, newLock)
		if sw.lockDiff.Changed(anyExceptHash) {
			sw.writeLock = true
		}
	} else if newLock != nil {
		sw.writeLock = true
	}

	switch vendor {
	case VendorAlways:
		sw.writeVendor = true
	case VendorOnChanged:
		if newLock != nil && oldLock == nil {
			sw.writeVendor = true
		} else if sw.lockDiff.Changed(anyExceptHash & ^verify.InputImportsChanged) {
			sw.writeVendor = true
		} else {
			for _, stat := range status {
				if stat != verify.NoMismatch {
					sw.writeVendor = true
					break
				}
			}
		}
	}

	if sw.writeVendor && newLock == nil {
		return nil, errors.New("must provide newLock in order to write out vendor")
	}

	return sw, nil
}

// HasLock checks if a Lock is present in the SafeWriter
func (sw *SafeWriter) HasLock() bool {
	return sw.lock != nil
}

// HasManifest checks if a Manifest is present in the SafeWriter
func (sw *SafeWriter) HasManifest() bool {
	return sw.Manifest != nil
}

// VendorBehavior defines when the vendor directory should be written.
type VendorBehavior int

const (
	// VendorOnChanged indicates that the vendor directory should be written
	// when the lock is new or changed, or a project in vendor differs from its
	// intended state.
	VendorOnChanged VendorBehavior = iota
	// VendorAlways forces the vendor directory to always be written.
	VendorAlways
	// VendorNever indicates the vendor directory should never be written.
	VendorNever
)

func (sw SafeWriter) validate(root string, sm gps.SourceManager) error {
	if root == "" {
		return errors.New("root path must be non-empty")
	}
	if is, err := fs.IsDir(root); !is {
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		return errors.Errorf("root path %q does not exist", root)
	}

	if sw.writeVendor && sm == nil {
		return errors.New("must provide a SourceManager if writing out a vendor dir")
	}

	return nil
}

// Write saves some combination of manifest, lock, and a vendor tree. root is
// the absolute path of root dir in which to write. sm is only required if
// vendor is being written.
//
// It first writes to a temp dir, then moves them in place if and only if all
// the write operations succeeded. It also does its best to roll back if any
// moves fail. This mostly guarantees that dep cannot exit with a partial write
// that would leave an undefined state on disk.
//
// If logger is not nil, progress will be logged after each project write.
func (sw *SafeWriter) Write(root string, sm gps.SourceManager, examples bool, logger *log.Logger) error {
	err := sw.validate(root, sm)
	if err != nil {
		return err
	}

	if !sw.HasManifest() && !sw.writeLock && !sw.writeVendor {
		// nothing to do
		return nil
	}

	mpath := filepath.Join(root, ManifestName)
	lpath := filepath.Join(root, LockName)
	vpath := filepath.Join(root, "vendor")

	td, err := ioutil.TempDir(os.TempDir(), "dep")
	if err != nil {
		return errors.Wrap(err, "error while creating temp dir for writing manifest/lock/vendor")
	}
	defer os.RemoveAll(td)

	if sw.HasManifest() {
		// Always write the example text to the bottom of the TOML file.
		tb, err := sw.Manifest.MarshalTOML()
		if err != nil {
			return errors.Wrap(err, "failed to marshal manifest to TOML")
		}

		var initOutput []byte

		// If examples are enabled, use the example text
		if examples {
			initOutput = exampleTOML
		}

		if err = ioutil.WriteFile(filepath.Join(td, ManifestName), append(initOutput, tb...), 0666); err != nil {
			return errors.Wrap(err, "failed to write manifest file to temp dir")
		}
	}

	if sw.writeVendor {
		var onWrite func(gps.WriteProgress)
		if logger != nil {
			onWrite = func(progress gps.WriteProgress) {
				logger.Println(progress)
			}
		}
		err = gps.WriteDepTree(filepath.Join(td, "vendor"), sw.lock, sm, sw.pruneOptions, onWrite)
		if err != nil {
			return errors.Wrap(err, "error while writing out vendor tree")
		}

		for k, lp := range sw.lock.Projects() {
			vp := lp.(verify.VerifiableProject)
			vp.Digest, err = verify.DigestFromDirectory(filepath.Join(td, "vendor", string(lp.Ident().ProjectRoot)))
			if err != nil {
				return errors.Wrapf(err, "error while hashing tree of %s in vendor", lp.Ident().ProjectRoot)
			}
			sw.lock.P[k] = vp
		}
	}

	if sw.writeLock {
		l, err := sw.lock.MarshalTOML()
		if err != nil {
			return errors.Wrap(err, "failed to marshal lock to TOML")
		}

		if err = ioutil.WriteFile(filepath.Join(td, LockName), append(lockFileComment, l...), 0666); err != nil {
			return errors.Wrap(err, "failed to write lock file to temp dir")
		}
	}

	// Ensure vendor/.git is preserved if present
	if hasDotGit(vpath) {
		err = fs.RenameWithFallback(filepath.Join(vpath, ".git"), filepath.Join(td, "vendor/.git"))
		if _, ok := err.(*os.LinkError); ok {
			return errors.Wrap(err, "failed to preserve vendor/.git")
		}
	}

	// Move the existing files and dirs to the temp dir while we put the new
	// ones in, to provide insurance against errors for as long as possible.
	type pathpair struct {
		from, to string
	}
	var restore []pathpair
	var failerr error
	var vendorbak string

	if sw.HasManifest() {
		if _, err := os.Stat(mpath); err == nil {
			// Move out the old one.
			tmploc := filepath.Join(td, ManifestName+".orig")
			failerr = fs.RenameWithFallback(mpath, tmploc)
			if failerr != nil {
				goto fail
			}
			restore = append(restore, pathpair{from: tmploc, to: mpath})
		}

		// Move in the new one.
		failerr = fs.RenameWithFallback(filepath.Join(td, ManifestName), mpath)
		if failerr != nil {
			goto fail
		}
	}

	if sw.writeLock {
		if _, err := os.Stat(lpath); err == nil {
			// Move out the old one.
			tmploc := filepath.Join(td, LockName+".orig")

			failerr = fs.RenameWithFallback(lpath, tmploc)
			if failerr != nil {
				goto fail
			}
			restore = append(restore, pathpair{from: tmploc, to: lpath})
		}

		// Move in the new one.
		failerr = fs.RenameWithFallback(filepath.Join(td, LockName), lpath)
		if failerr != nil {
			goto fail
		}
	}

	if sw.writeVendor {
		if _, err := os.Stat(vpath); err == nil {
			// Move out the old vendor dir. just do it into an adjacent dir, to
			// try to mitigate the possibility of a pointless cross-filesystem
			// move with a temp directory.
			vendorbak = vpath + ".orig"
			if _, err := os.Stat(vendorbak); err == nil {
				// If the adjacent dir already exists, bite the bullet and move
				// to a proper tempdir.
				vendorbak = filepath.Join(td, ".vendor.orig")
			}

			failerr = fs.RenameWithFallback(vpath, vendorbak)
			if failerr != nil {
				goto fail
			}
			restore = append(restore, pathpair{from: vendorbak, to: vpath})
		}

		// Move in the new one.
		failerr = fs.RenameWithFallback(filepath.Join(td, "vendor"), vpath)
		if failerr != nil {
			goto fail
		}
	}

	// Renames all went smoothly. The deferred os.RemoveAll will get the temp
	// dir, but if we wrote vendor, we have to clean that up directly
	if sw.writeVendor {
		// Nothing we can really do about an error at this point, so ignore it
		os.RemoveAll(vendorbak)
	}

	return nil

fail:
	// If we failed at any point, move all the things back into place, then bail.
	for _, pair := range restore {
		// Nothing we can do on err here, as we're already in recovery mode.
		fs.RenameWithFallback(pair.from, pair.to)
	}
	return failerr
}

// PrintPreparedActions logs the actions a call to Write would perform.
func (sw *SafeWriter) PrintPreparedActions(output *log.Logger, verbose bool) error {
	if output == nil {
		output = log.New(ioutil.Discard, "", 0)
	}
	if sw.HasManifest() {
		if verbose {
			m, err := sw.Manifest.MarshalTOML()
			if err != nil {
				return errors.Wrap(err, "ensure DryRun cannot serialize manifest")
			}
			output.Printf("Would have written the following %s:\n%s\n", ManifestName, string(m))
		} else {
			output.Printf("Would have written %s.\n", ManifestName)
		}
	}

	if sw.writeLock {
		if verbose {
			l, err := sw.lock.MarshalTOML()
			if err != nil {
				return errors.Wrap(err, "ensure DryRun cannot serialize lock")
			}
			output.Printf("Would have written the following %s:\n%s\n", LockName, string(l))
		} else {
			output.Printf("Would have written %s.\n", LockName)
		}
	}

	if sw.writeVendor {
		if verbose {
			output.Printf("Would have written the following %d projects to the vendor directory:\n", len(sw.lock.Projects()))
			lps := sw.lock.Projects()
			for i, p := range lps {
				output.Printf("(%d/%d) %s@%s\n", i+1, len(lps), p.Ident(), p.Version())
			}
		} else {
			output.Printf("Would have written %d projects to the vendor directory.\n", len(sw.lock.Projects()))
		}
	}

	return nil
}

// hasDotGit checks if a given path has .git file or directory in it.
func hasDotGit(path string) bool {
	gitfilepath := filepath.Join(path, ".git")
	_, err := os.Stat(gitfilepath)
	return err == nil
}

// DeltaWriter manages batched writes to populate vendor/ and update Gopkg.lock.
// Its primary design goal is to minimize writes by only writing things that
// have changed.
type DeltaWriter struct {
	lock      *Lock
	lockDiff  verify.LockDelta
	vendorDir string
	changed   map[gps.ProjectRoot]changeType
	behavior  VendorBehavior
}

type changeType uint8

const (
	hashMismatch changeType = iota + 1
	hashVersionMismatch
	hashAbsent
	noVerify
	solveChanged
	pruneOptsChanged
	missingFromTree
	projectAdded
	projectRemoved
)

// NewDeltaWriter prepares a vendor writer that will construct a vendor
// directory by writing out only those projects that actually need to be written
// out - they have changed in some way, or they lack the necessary hash
// information to be verified.
func NewDeltaWriter(p *Project, newLock *Lock, behavior VendorBehavior) (TreeWriter, error) {
	dw := &DeltaWriter{
		lock:      newLock,
		vendorDir: filepath.Join(p.AbsRoot, "vendor"),
		changed:   make(map[gps.ProjectRoot]changeType),
		behavior:  behavior,
	}

	if newLock == nil {
		return nil, errors.New("must provide a non-nil newlock")
	}

	status, err := p.VerifyVendor()
	if err != nil {
		return nil, err
	}

	_, err = os.Stat(dw.vendorDir)
	if err != nil {
		if os.IsNotExist(err) {
			// Provided dir does not exist, so there's no disk contents to compare
			// against. Fall back to the old SafeWriter.
			return NewSafeWriter(nil, p.Lock, newLock, behavior, p.Manifest.PruneOptions, status)
		}
		return nil, err
	}

	dw.lockDiff = verify.DiffLocks(p.Lock, newLock)

	for pr, lpd := range dw.lockDiff.ProjectDeltas {
		// Hash changes aren't relevant at this point, as they could be empty
		// in the new lock, and therefore a symptom of a solver change.
		if lpd.Changed(anyExceptHash) {
			if lpd.WasAdded() {
				dw.changed[pr] = projectAdded
			} else if lpd.WasRemoved() {
				dw.changed[pr] = projectRemoved
			} else if lpd.PruneOptsChanged() {
				dw.changed[pr] = pruneOptsChanged
			} else {
				dw.changed[pr] = solveChanged
			}
		}
	}

	for spr, stat := range status {
		pr := gps.ProjectRoot(spr)
		// These cases only matter if there was no change already recorded via
		// the differ.
		if _, has := dw.changed[pr]; !has {
			switch stat {
			case verify.NotInTree:
				dw.changed[pr] = missingFromTree
			case verify.NotInLock:
				dw.changed[pr] = projectRemoved
			case verify.DigestMismatchInLock:
				dw.changed[pr] = hashMismatch
			case verify.HashVersionMismatch:
				dw.changed[pr] = hashVersionMismatch
			case verify.EmptyDigestInLock:
				dw.changed[pr] = hashAbsent
			}
		}
	}

	// Apply noverify last, as it should only supersede changeTypes with lower
	// values. It is NOT applied if no existing change is registered.
	for _, spr := range p.Manifest.NoVerify {
		pr := gps.ProjectRoot(spr)
		// We don't validate this field elsewhere as it can be difficult to know
		// at the beginning of a dep ensure command whether or not the noverify
		// project actually will exist as part of the Lock by the end of the
		// run. So, only apply if it's in the lockdiff, and isn't a removal.
		if _, has := dw.lockDiff.ProjectDeltas[pr]; has {
			if typ, has := dw.changed[pr]; has && typ < noVerify {
				// Avoid writing noverify projects at all for the lower change
				// types.
				delete(dw.changed, pr)

				// Uncomment this if we want to switch to the safer behavior,
				// where we ALWAYS write noverify projects.
				//dw.changed[pr] = noVerify
			}
		}
	}

	return dw, nil
}

// Write executes the planned changes.
//
// This writes recreated projects to a new directory, then moves in existing,
// unchanged projects from the original vendor directory. If any failures occur,
// reasonable attempts are made to roll back the changes.
func (dw *DeltaWriter) Write(path string, sm gps.SourceManager, examples bool, logger *log.Logger) error {
	// TODO(sdboyer) remove path from the signature for this
	if path != filepath.Dir(dw.vendorDir) {
		return errors.Errorf("target path (%q) must be the parent of the original vendor path (%q)", path, dw.vendorDir)
	}

	if logger == nil {
		logger = log.New(ioutil.Discard, "", 0)
	}

	lpath := filepath.Join(path, LockName)
	vpath := dw.vendorDir

	// Write the modified projects to a new adjacent directory. We use an
	// adjacent directory to minimize the possibility of cross-filesystem renames
	// becoming expensive copies, and to make removal of unneeded projects implicit
	// and automatic.
	vnewpath := filepath.Join(filepath.Dir(vpath), ".vendor-new")
	if _, err := os.Stat(vnewpath); err == nil {
		return errors.Errorf("scratch directory %s already exists, please remove it", vnewpath)
	}
	err := os.MkdirAll(vnewpath, os.FileMode(0777))
	if err != nil {
		return errors.Wrapf(err, "error while creating scratch directory at %s", vnewpath)
	}

	// Write out all the deltas to the newpath
	projs := make(map[gps.ProjectRoot]gps.LockedProject)
	for _, lp := range dw.lock.Projects() {
		projs[lp.Ident().ProjectRoot] = lp
	}

	dropped := []gps.ProjectRoot{}
	i := 0
	tot := len(dw.changed)
	if len(dw.changed) > 0 {
		logger.Println("# Bringing vendor into sync")
	}
	for pr, reason := range dw.changed {
		if reason == projectRemoved {
			dropped = append(dropped, pr)
			continue
		}

		to := filepath.FromSlash(filepath.Join(vnewpath, string(pr)))
		proj, has := projs[pr]
		if !has {
			// This shouldn't be reachable, but it's preferable to print an
			// error and continue rather than panic. https://github.com/golang/dep/issues/1945
			// TODO(sdboyer) remove this once we've increased confidence around
			// this case.
			fmt.Fprintf(os.Stderr, "Internal error - %s had change code %v but was not in new Gopkg.lock. Re-running dep ensure should fix this. Please file a bug at https://github.com/golang/dep/issues/new!\n", pr, reason)
			continue
		}
		po := proj.(verify.VerifiableProject).PruneOpts
		if err := sm.ExportPrunedProject(context.TODO(), projs[pr], po, to); err != nil {
			return errors.Wrapf(err, "failed to export %s", pr)
		}

		i++
		lpd := dw.lockDiff.ProjectDeltas[pr]
		v, id := projs[pr].Version(), projs[pr].Ident()

		// Only print things if we're actually going to leave behind a new
		// vendor dir.
		if dw.behavior != VendorNever {
			logger.Printf("(%d/%d) Wrote %s@%s: %s", i, tot, id, v, changeExplanation(reason, lpd))
		}

		digest, err := verify.DigestFromDirectory(to)
		if err != nil {
			return errors.Wrapf(err, "failed to hash %s", pr)
		}

		// Update the new Lock with verification information.
		for k, lp := range dw.lock.P {
			if lp.Ident().ProjectRoot == pr {
				vp := lp.(verify.VerifiableProject)
				vp.Digest = digest
				dw.lock.P[k] = verify.VerifiableProject{
					LockedProject: lp,
					PruneOpts:     po,
					Digest:        digest,
				}
			}
		}
	}

	// Write out the lock, now that it's fully updated with digests.
	l, err := dw.lock.MarshalTOML()
	if err != nil {
		return errors.Wrap(err, "failed to marshal lock to TOML")
	}

	if err = ioutil.WriteFile(lpath, append(lockFileComment, l...), 0666); err != nil {
		return errors.Wrap(err, "failed to write new lock file")
	}

	if dw.behavior == VendorNever {
		return os.RemoveAll(vnewpath)
	}

	// Changed projects are fully populated. Now, iterate over the lock's
	// projects and move any remaining ones not in the changed list to vnewpath.
	for _, lp := range dw.lock.Projects() {
		pr := lp.Ident().ProjectRoot
		tgt := filepath.Join(vnewpath, string(pr))
		err := os.MkdirAll(filepath.Dir(tgt), os.FileMode(0777))
		if err != nil {
			return errors.Wrapf(err, "error creating parent directory in vendor for %s", tgt)
		}

		if _, has := dw.changed[pr]; !has {
			err = fs.RenameWithFallback(filepath.Join(vpath, string(pr)), tgt)
			if err != nil {
				return errors.Wrapf(err, "error moving unchanged project %s into scratch vendor dir", pr)
			}
		}
	}

	for i, pr := range dropped {
		// Kind of a lie to print this. ¯\_(ツ)_/¯
		fi, err := os.Stat(filepath.Join(vpath, string(pr)))
		if err != nil {
			return errors.Wrap(err, "could not stat file that VerifyVendor claimed existed")
		}

		if fi.IsDir() {
			logger.Printf("(%d/%d) Removed unused project %s", tot-(len(dropped)-i-1), tot, pr)
		} else {
			logger.Printf("(%d/%d) Removed orphaned file %s", tot-(len(dropped)-i-1), tot, pr)
		}
	}

	// Ensure vendor/.git is preserved if present
	if hasDotGit(vpath) {
		err = fs.RenameWithFallback(filepath.Join(vpath, ".git"), filepath.Join(vnewpath, "vendor/.git"))
		if _, ok := err.(*os.LinkError); ok {
			return errors.Wrap(err, "failed to preserve vendor/.git")
		}
	}
	err = os.RemoveAll(vpath)
	if err != nil {
		return errors.Wrap(err, "failed to remove original vendor directory")
	}
	err = fs.RenameWithFallback(vnewpath, vpath)
	if err != nil {
		return errors.Wrap(err, "failed to put new vendor directory into place")
	}

	return nil
}

// changeExplanation outputs a string explaining what changed for each different
// possible changeType.
func changeExplanation(c changeType, lpd verify.LockedProjectDelta) string {
	switch c {
	case noVerify:
		return "verification is disabled"
	case solveChanged:
		if lpd.SourceChanged() {
			return fmt.Sprintf("source changed (%s -> %s)", lpd.SourceBefore, lpd.SourceAfter)
		} else if lpd.VersionChanged() {
			if lpd.VersionBefore == nil {
				return fmt.Sprintf("version changed (was a bare revision)")
			}
			return fmt.Sprintf("version changed (was %s)", lpd.VersionBefore.String())
		} else if lpd.RevisionChanged() {
			return fmt.Sprintf("revision changed (%s -> %s)", trimSHA(lpd.RevisionBefore), trimSHA(lpd.RevisionAfter))
		} else if lpd.PackagesChanged() {
			la, lr := len(lpd.PackagesAdded), len(lpd.PackagesRemoved)
			if la > 0 && lr > 0 {
				return fmt.Sprintf("packages changed (%v added, %v removed)", la, lr)
			} else if la > 0 {
				return fmt.Sprintf("packages changed (%v added)", la)
			}
			return fmt.Sprintf("packages changed (%v removed)", lr)
		}
	case pruneOptsChanged:
		// Override what's on the lockdiff with the extra info we have;
		// this lets us excise PruneNestedVendorDirs and get the real
		// value from the input param in place.
		old := lpd.PruneOptsBefore & ^gps.PruneNestedVendorDirs
		new := lpd.PruneOptsAfter & ^gps.PruneNestedVendorDirs
		return fmt.Sprintf("prune options changed (%s -> %s)", old, new)
	case hashMismatch:
		return "hash of vendored tree didn't match digest in Gopkg.lock"
	case hashVersionMismatch:
		return "hashing algorithm mismatch"
	case hashAbsent:
		return "hash digest absent from lock"
	case projectAdded:
		return "new project"
	case missingFromTree:
		return "missing from vendor"
	default:
		panic(fmt.Sprintf("unrecognized changeType value %v", c))
	}

	return ""
}

// PrintPreparedActions indicates what changes the DeltaWriter plans to make.
func (dw *DeltaWriter) PrintPreparedActions(output *log.Logger, verbose bool) error {
	if verbose {
		l, err := dw.lock.MarshalTOML()
		if err != nil {
			return errors.Wrap(err, "ensure DryRun cannot serialize lock")
		}
		output.Printf("Would have written the following %s (hash digests may be incorrect):\n%s\n", LockName, string(l))
	} else {
		output.Printf("Would have written %s.\n", LockName)
	}

	projs := make(map[gps.ProjectRoot]gps.LockedProject)
	for _, lp := range dw.lock.Projects() {
		projs[lp.Ident().ProjectRoot] = lp
	}

	tot := len(dw.changed)
	if tot > 0 {
		output.Print("Would have updated the following projects in the vendor directory:\n\n")
		i := 0
		for pr, reason := range dw.changed {
			lpd := dw.lockDiff.ProjectDeltas[pr]
			if reason == projectRemoved {
				output.Printf("(%d/%d) Would have removed %s", i, tot, pr)
			} else {
				output.Printf("(%d/%d) Would have written %s@%s: %s", i, tot, projs[pr].Ident(), projs[pr].Version(), changeExplanation(reason, lpd))
			}
		}
	}

	return nil
}

// A TreeWriter is responsible for writing important dep states to disk -
// Gopkg.lock, vendor, and possibly Gopkg.toml.
type TreeWriter interface {
	PrintPreparedActions(output *log.Logger, verbose bool) error
	Write(path string, sm gps.SourceManager, examples bool, logger *log.Logger) error
}

// trimSHA checks if revision is a valid SHA1 digest and trims to 10 characters.
func trimSHA(revision gps.Revision) string {
	if len(revision) == 40 {
		if _, err := hex.DecodeString(string(revision)); err == nil {
			// Valid SHA1 digest
			revision = revision[0:10]
		}
	}

	return string(revision)
}
