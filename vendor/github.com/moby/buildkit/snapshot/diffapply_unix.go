//go:build !windows
// +build !windows

package snapshot

import (
	"context"
	gofs "io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/containerd/containerd/leases"
	"github.com/containerd/containerd/mount"
	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/continuity/fs"
	"github.com/containerd/continuity/sysx"
	"github.com/containerd/stargz-snapshotter/snapshot/overlayutils"
	"github.com/hashicorp/go-multierror"
	"github.com/moby/buildkit/identity"
	"github.com/moby/buildkit/util/bklog"
	"github.com/moby/buildkit/util/leaseutil"
	"github.com/moby/buildkit/util/overlay"
	"github.com/pkg/errors"
	"golang.org/x/sys/unix"
)

// diffApply applies the provided diffs to the dest Mountable and returns the correctly calculated disk usage
// that accounts for any hardlinks made from existing snapshots. ctx is expected to have a temporary lease
// associated with it.
func (sn *mergeSnapshotter) diffApply(ctx context.Context, dest Mountable, diffs ...Diff) (_ snapshots.Usage, rerr error) {
	a, err := applierFor(dest, sn.tryCrossSnapshotLink)
	if err != nil {
		return snapshots.Usage{}, errors.Wrapf(err, "failed to create applier")
	}
	defer func() {
		releaseErr := a.Release()
		if releaseErr != nil {
			rerr = multierror.Append(rerr, errors.Wrapf(releaseErr, "failed to release applier")).ErrorOrNil()
		}
	}()

	// TODO:(sipsma) optimization: parallelize differ and applier in separate goroutines, connected with a buffered channel

	for _, diff := range diffs {
		var lowerMntable Mountable
		if diff.Lower != "" {
			if info, err := sn.Stat(ctx, diff.Lower); err != nil {
				return snapshots.Usage{}, errors.Wrapf(err, "failed to stat lower snapshot %s", diff.Lower)
			} else if info.Kind == snapshots.KindCommitted {
				lowerMntable, err = sn.View(ctx, identity.NewID(), diff.Lower)
				if err != nil {
					return snapshots.Usage{}, errors.Wrapf(err, "failed to mount lower snapshot view %s", diff.Lower)
				}
			} else {
				lowerMntable, err = sn.Mounts(ctx, diff.Lower)
				if err != nil {
					return snapshots.Usage{}, errors.Wrapf(err, "failed to mount lower snapshot %s", diff.Lower)
				}
			}
		}
		var upperMntable Mountable
		if diff.Upper != "" {
			if info, err := sn.Stat(ctx, diff.Upper); err != nil {
				return snapshots.Usage{}, errors.Wrapf(err, "failed to stat upper snapshot %s", diff.Upper)
			} else if info.Kind == snapshots.KindCommitted {
				upperMntable, err = sn.View(ctx, identity.NewID(), diff.Upper)
				if err != nil {
					return snapshots.Usage{}, errors.Wrapf(err, "failed to mount upper snapshot view %s", diff.Upper)
				}
			} else {
				upperMntable, err = sn.Mounts(ctx, diff.Upper)
				if err != nil {
					return snapshots.Usage{}, errors.Wrapf(err, "failed to mount upper snapshot %s", diff.Upper)
				}
			}
		} else {
			// create an empty view
			upperMntable, err = sn.View(ctx, identity.NewID(), "")
			if err != nil {
				return snapshots.Usage{}, errors.Wrapf(err, "failed to mount empty upper snapshot view %s", diff.Upper)
			}
		}
		d, err := differFor(lowerMntable, upperMntable)
		if err != nil {
			return snapshots.Usage{}, errors.Wrapf(err, "failed to create differ")
		}
		defer func() {
			rerr = multierror.Append(rerr, d.Release()).ErrorOrNil()
		}()
		if err := d.HandleChanges(ctx, a.Apply); err != nil {
			return snapshots.Usage{}, errors.Wrapf(err, "failed to handle changes")
		}
	}

	if err := a.Flush(); err != nil {
		return snapshots.Usage{}, errors.Wrapf(err, "failed to flush changes")
	}
	return a.Usage()
}

type change struct {
	kind    fs.ChangeKind
	subpath string
	srcpath string
	srcStat *syscall.Stat_t
	// linkSubpath is set to a subpath of a previous change from the same
	// differ instance that is a hardlink to this one, if any.
	linkSubpath string
}

type changeApply struct {
	*change
	dstpath string
	dstStat *syscall.Stat_t
}

type inode struct {
	ino uint64
	dev uint64
}

func statInode(stat *syscall.Stat_t) inode {
	if stat == nil {
		return inode{}
	}
	return inode{
		ino: stat.Ino,
		dev: stat.Dev,
	}
}

type applier struct {
	root                 string
	release              func() error
	lowerdirs            []string // ordered highest -> lowest, the order we want to check them in
	crossSnapshotLinks   map[inode]struct{}
	createWhiteoutDelete bool
	dirModTimes          map[string]unix.Timespec // map of dstpath -> mtime that should be set on that subpath
}

func applierFor(dest Mountable, tryCrossSnapshotLink bool) (_ *applier, rerr error) {
	app := &applier{
		dirModTimes: make(map[string]unix.Timespec),
	}
	defer func() {
		if rerr != nil {
			rerr = multierror.Append(rerr, app.Release()).ErrorOrNil()
		}
	}()
	if tryCrossSnapshotLink {
		app.crossSnapshotLinks = make(map[inode]struct{})
	}

	mnts, release, err := dest.Mount()
	if err != nil {
		return nil, nil
	}
	app.release = release

	if len(mnts) != 1 {
		return nil, errors.Errorf("expected exactly one mount, got %d", len(mnts))
	}
	mnt := mnts[0]

	switch mnt.Type {
	case "overlay":
		for _, opt := range mnt.Options {
			if strings.HasPrefix(opt, "upperdir=") {
				app.root = strings.TrimPrefix(opt, "upperdir=")
			} else if strings.HasPrefix(opt, "lowerdir=") {
				app.lowerdirs = strings.Split(strings.TrimPrefix(opt, "lowerdir="), ":")
			}
		}
		if app.root == "" {
			return nil, errors.Errorf("could not find upperdir in mount options %v", mnt.Options)
		}
		if len(app.lowerdirs) == 0 {
			return nil, errors.Errorf("could not find lowerdir in mount options %v", mnt.Options)
		}
		app.createWhiteoutDelete = true
	case "bind", "rbind":
		app.root = mnt.Source
	default:
		mnter := LocalMounter(dest)
		root, err := mnter.Mount()
		if err != nil {
			return nil, err
		}
		app.root = root
		prevRelease := app.release
		app.release = func() error {
			err := mnter.Unmount()
			return multierror.Append(err, prevRelease()).ErrorOrNil()
		}
	}

	app.root, err = filepath.EvalSymlinks(app.root)
	if err != nil {
		return nil, errors.Wrapf(err, "failed to resolve symlinks in %s", app.root)
	}
	return app, nil
}

func (a *applier) Apply(ctx context.Context, c *change) error {
	if c == nil {
		return errors.New("nil change")
	}

	if c.kind == fs.ChangeKindUnmodified {
		return nil
	}

	dstpath, err := safeJoin(a.root, c.subpath)
	if err != nil {
		return errors.Wrapf(err, "failed to join paths %q and %q", a.root, c.subpath)
	}
	var dstStat *syscall.Stat_t
	if dstfi, err := os.Lstat(dstpath); err == nil {
		stat, ok := dstfi.Sys().(*syscall.Stat_t)
		if !ok {
			return errors.Errorf("failed to get stat_t for %T", dstStat)
		}
		dstStat = stat
	} else if !os.IsNotExist(err) {
		return errors.Wrap(err, "failed to stat during copy apply")
	}

	ca := &changeApply{
		change:  c,
		dstpath: dstpath,
		dstStat: dstStat,
	}

	if done, err := a.applyDelete(ctx, ca); err != nil {
		return errors.Wrap(err, "failed to delete during apply")
	} else if done {
		return nil
	}

	if done, err := a.applyHardlink(ctx, ca); err != nil {
		return errors.Wrapf(err, "failed to hardlink during apply")
	} else if done {
		return nil
	}

	if err := a.applyCopy(ctx, ca); err != nil {
		return errors.Wrapf(err, "failed to copy during apply")
	}
	return nil
}

func (a *applier) applyDelete(ctx context.Context, ca *changeApply) (bool, error) {
	// Even when not deleting, we may be overwriting a file, in which case we should
	// delete the existing file at the path, if any. Don't delete when both are dirs
	// in this case though because they should get merged, not overwritten.
	deleteOnly := ca.kind == fs.ChangeKindDelete
	overwrite := !deleteOnly && ca.dstStat != nil && ca.srcStat.Mode&ca.dstStat.Mode&unix.S_IFMT != unix.S_IFDIR

	if !deleteOnly && !overwrite {
		// nothing to delete, continue on
		return false, nil
	}

	if err := os.RemoveAll(ca.dstpath); err != nil {
		return false, errors.Wrap(err, "failed to remove during apply")
	}
	ca.dstStat = nil

	if deleteOnly && a.createWhiteoutDelete {
		// only create a whiteout device if there is something to delete
		var foundLower bool
		for _, lowerdir := range a.lowerdirs {
			lowerpath, err := safeJoin(lowerdir, ca.subpath)
			if err != nil {
				return false, errors.Wrapf(err, "failed to join lowerdir %q and subpath %q", lowerdir, ca.subpath)
			}
			if _, err := os.Lstat(lowerpath); err == nil {
				foundLower = true
				break
			} else if !errors.Is(err, unix.ENOENT) && !errors.Is(err, unix.ENOTDIR) {
				return false, errors.Wrapf(err, "failed to stat lowerpath %q", lowerpath)
			}
		}
		if foundLower {
			ca.kind = fs.ChangeKindAdd
			if ca.srcStat == nil {
				ca.srcStat = &syscall.Stat_t{
					Mode: syscall.S_IFCHR,
					Rdev: unix.Mkdev(0, 0),
				}
				ca.srcpath = ""
			}
			return false, nil
		}
	}

	return deleteOnly, nil
}

func (a *applier) applyHardlink(ctx context.Context, ca *changeApply) (bool, error) {
	switch ca.srcStat.Mode & unix.S_IFMT {
	case unix.S_IFDIR, unix.S_IFIFO, unix.S_IFSOCK:
		// Directories can't be hard-linked, so they just have to be recreated.
		// Named pipes and sockets can be hard-linked but is best to avoid as it could enable IPC in weird cases.
		return false, nil

	default:
		var linkSrcpath string
		if ca.linkSubpath != "" {
			// there's an already applied path that we should link from
			path, err := safeJoin(a.root, ca.linkSubpath)
			if err != nil {
				return false, errors.Errorf("failed to get hardlink source path: %v", err)
			}
			linkSrcpath = path
		} else if a.crossSnapshotLinks != nil {
			// we can try to link across snapshots from the source file
			linkSrcpath = ca.srcpath
			a.crossSnapshotLinks[statInode(ca.srcStat)] = struct{}{}
		}
		if linkSrcpath == "" {
			// nothing to hardlink from, will have to copy the file
			return false, nil
		}

		if err := os.Link(linkSrcpath, ca.dstpath); errors.Is(err, unix.EXDEV) || errors.Is(err, unix.EMLINK) {
			// These errors are expected when the hardlink would cross devices or would exceed the maximum number of links for the inode.
			// Just fallback to a copy.
			bklog.G(ctx).WithError(err).WithField("srcpath", linkSrcpath).WithField("dstpath", ca.dstpath).Debug("hardlink failed")
			if a.crossSnapshotLinks != nil {
				delete(a.crossSnapshotLinks, statInode(ca.srcStat))
			}
			return false, nil
		} else if err != nil {
			return false, errors.Wrap(err, "failed to hardlink during apply")
		}

		return true, nil
	}
}

func (a *applier) applyCopy(ctx context.Context, ca *changeApply) error {
	switch ca.srcStat.Mode & unix.S_IFMT {
	case unix.S_IFREG:
		if err := fs.CopyFile(ca.dstpath, ca.srcpath); err != nil {
			return errors.Wrapf(err, "failed to copy from %s to %s during apply", ca.srcpath, ca.dstpath)
		}
	case unix.S_IFDIR:
		if ca.dstStat == nil {
			// dstpath doesn't exist, make it a dir
			if err := unix.Mkdir(ca.dstpath, ca.srcStat.Mode); err != nil {
				return errors.Wrapf(err, "failed to create applied dir at %q from %q", ca.dstpath, ca.srcpath)
			}
		}
	case unix.S_IFLNK:
		if target, err := os.Readlink(ca.srcpath); err != nil {
			return errors.Wrap(err, "failed to read symlink during apply")
		} else if err := os.Symlink(target, ca.dstpath); err != nil {
			return errors.Wrap(err, "failed to create symlink during apply")
		}
	case unix.S_IFBLK, unix.S_IFCHR, unix.S_IFIFO, unix.S_IFSOCK:
		if err := unix.Mknod(ca.dstpath, ca.srcStat.Mode, int(ca.srcStat.Rdev)); err != nil {
			return errors.Wrap(err, "failed to mknod during apply")
		}
	default:
		// should never be here, all types should be handled
		return errors.Errorf("unhandled file type %d during merge at path %q", ca.srcStat.Mode&unix.S_IFMT, ca.srcpath)
	}

	if ca.srcpath != "" {
		xattrs, err := sysx.LListxattr(ca.srcpath)
		if err != nil {
			return errors.Wrapf(err, "failed to list xattrs of src path %s", ca.srcpath)
		}
		for _, xattr := range xattrs {
			if isOpaqueXattr(xattr) {
				// Don't recreate opaque xattrs during merge. These should only be set when using overlay snapshotters,
				// in which case we are converting from the "opaque whiteout" format to the "explicit whiteout" format during
				// the merge (as taken care of by the overlay differ).
				continue
			}
			xattrVal, err := sysx.LGetxattr(ca.srcpath, xattr)
			if err != nil {
				return errors.Wrapf(err, "failed to get xattr %s of src path %s", xattr, ca.srcpath)
			}
			if err := sysx.LSetxattr(ca.dstpath, xattr, xattrVal, 0); err != nil {
				// This can often fail, so just log it: https://github.com/moby/buildkit/issues/1189
				bklog.G(ctx).Debugf("failed to set xattr %s of path %s during apply", xattr, ca.dstpath)
			}
		}
	}

	if err := os.Lchown(ca.dstpath, int(ca.srcStat.Uid), int(ca.srcStat.Gid)); err != nil {
		return errors.Wrap(err, "failed to chown during apply")
	}

	if ca.srcStat.Mode&unix.S_IFMT != unix.S_IFLNK {
		if err := unix.Chmod(ca.dstpath, ca.srcStat.Mode); err != nil {
			return errors.Wrapf(err, "failed to chmod path %q during apply", ca.dstpath)
		}
	}

	atimeSpec := unix.Timespec{Sec: ca.srcStat.Atim.Sec, Nsec: ca.srcStat.Atim.Nsec}
	mtimeSpec := unix.Timespec{Sec: ca.srcStat.Mtim.Sec, Nsec: ca.srcStat.Mtim.Nsec}
	if ca.srcStat.Mode&unix.S_IFMT != unix.S_IFDIR {
		// apply times immediately for non-dirs
		if err := unix.UtimesNanoAt(unix.AT_FDCWD, ca.dstpath, []unix.Timespec{atimeSpec, mtimeSpec}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			return err
		}
	} else {
		// save the times we should set on this dir, to be applied after subfiles have been set
		a.dirModTimes[ca.dstpath] = mtimeSpec
	}

	return nil
}

func (a *applier) Flush() error {
	// Set dir times now that everything has been modified. Walk the filesystem tree to ensure
	// that we never try to apply to a path that has been deleted or modified since times for it
	// were stored. This is needed for corner cases such as where a parent dir is removed and
	// replaced with a symlink.
	return filepath.WalkDir(a.root, func(path string, d gofs.DirEntry, prevErr error) error {
		if prevErr != nil {
			return prevErr
		}
		if !d.IsDir() {
			return nil
		}
		if mtime, ok := a.dirModTimes[path]; ok {
			if err := unix.UtimesNanoAt(unix.AT_FDCWD, path, []unix.Timespec{{Nsec: unix.UTIME_OMIT}, mtime}, unix.AT_SYMLINK_NOFOLLOW); err != nil {
				return err
			}
		}
		return nil
	})
}

func (a *applier) Release() error {
	if a.release != nil {
		err := a.release()
		if err != nil {
			return err
		}
	}
	a.release = nil
	return nil
}

func (a *applier) Usage() (snapshots.Usage, error) {
	// Calculate the disk space used under the apply root, similar to the normal containerd snapshotter disk usage
	// calculations but with the extra ability to take into account hardlinks that were created between snapshots, ensuring that
	// they don't get double counted.
	inodes := make(map[inode]struct{})
	var usage snapshots.Usage
	if err := filepath.WalkDir(a.root, func(path string, dirent gofs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := dirent.Info()
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		inode := statInode(stat)
		if _, ok := inodes[inode]; ok {
			return nil
		}
		inodes[inode] = struct{}{}
		if a.crossSnapshotLinks != nil {
			if _, ok := a.crossSnapshotLinks[statInode(stat)]; ok {
				// don't count cross-snapshot hardlinks
				return nil
			}
		}
		usage.Inodes++
		usage.Size += stat.Blocks * 512 // 512 is always block size, see "man 2 stat"
		return nil
	}); err != nil {
		return snapshots.Usage{}, err
	}
	return usage, nil
}

type differ struct {
	lowerRoot    string
	releaseLower func() error

	upperRoot    string
	releaseUpper func() error

	upperBindSource  string
	upperOverlayDirs []string // ordered lowest -> highest

	upperdir string

	visited map[string]struct{} // set of parent subpaths that have been visited
	inodes  map[inode]string    // map of inode -> subpath
}

func differFor(lowerMntable, upperMntable Mountable) (_ *differ, rerr error) {
	d := &differ{
		visited: make(map[string]struct{}),
		inodes:  make(map[inode]string),
	}
	defer func() {
		if rerr != nil {
			rerr = multierror.Append(rerr, d.Release()).ErrorOrNil()
		}
	}()

	var lowerMnts []mount.Mount
	if lowerMntable != nil {
		mnts, release, err := lowerMntable.Mount()
		if err != nil {
			return nil, err
		}
		mounter := LocalMounterWithMounts(mnts)
		root, err := mounter.Mount()
		if err != nil {
			return nil, err
		}
		d.lowerRoot = root
		lowerMnts = mnts
		d.releaseLower = func() error {
			err := mounter.Unmount()
			return multierror.Append(err, release()).ErrorOrNil()
		}
	}

	var upperMnts []mount.Mount
	if upperMntable != nil {
		mnts, release, err := upperMntable.Mount()
		if err != nil {
			return nil, err
		}
		mounter := LocalMounterWithMounts(mnts)
		root, err := mounter.Mount()
		if err != nil {
			return nil, err
		}
		d.upperRoot = root
		upperMnts = mnts
		d.releaseUpper = func() error {
			err := mounter.Unmount()
			return multierror.Append(err, release()).ErrorOrNil()
		}
	}

	if len(upperMnts) == 1 {
		switch upperMnts[0].Type {
		case "bind", "rbind":
			d.upperBindSource = upperMnts[0].Source
		case "overlay":
			overlayDirs, err := overlay.GetOverlayLayers(upperMnts[0])
			if err != nil {
				return nil, errors.Wrapf(err, "failed to get overlay layers from mount %+v", upperMnts[0])
			}
			d.upperOverlayDirs = overlayDirs
		}
	}
	if len(lowerMnts) > 0 {
		if upperdir, err := overlay.GetUpperdir(lowerMnts, upperMnts); err == nil {
			d.upperdir = upperdir
		}
	}

	return d, nil
}

func (d *differ) HandleChanges(ctx context.Context, handle func(context.Context, *change) error) error {
	if d.upperdir != "" {
		return d.overlayChanges(ctx, handle)
	}
	return d.doubleWalkingChanges(ctx, handle)
}

func (d *differ) doubleWalkingChanges(ctx context.Context, handle func(context.Context, *change) error) error {
	return fs.Changes(ctx, d.lowerRoot, d.upperRoot, func(kind fs.ChangeKind, subpath string, srcfi os.FileInfo, prevErr error) error {
		if prevErr != nil {
			return prevErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if kind == fs.ChangeKindUnmodified {
			return nil
		}

		// NOTE: it's tempting to skip creating parent dirs when change kind is Delete, but
		// that would make us incompatible with the image exporter code:
		// https://github.com/containerd/containerd/pull/2095
		if err := d.checkParent(ctx, subpath, handle); err != nil {
			return errors.Wrapf(err, "failed to check parent for %s", subpath)
		}

		c := &change{
			kind:    kind,
			subpath: subpath,
		}

		if srcfi != nil {
			// Try to ensure that srcpath and srcStat are set to a file from the underlying filesystem
			// rather than the actual mount when possible. This allows hardlinking without getting EXDEV.
			switch {
			case !srcfi.IsDir() && d.upperBindSource != "":
				srcpath, err := safeJoin(d.upperBindSource, c.subpath)
				if err != nil {
					return errors.Wrapf(err, "failed to join %s and %s", d.upperBindSource, c.subpath)
				}
				c.srcpath = srcpath
				if fi, err := os.Lstat(c.srcpath); err == nil {
					srcfi = fi
				} else {
					return errors.Wrap(err, "failed to stat underlying file from bind mount")
				}
			case !srcfi.IsDir() && len(d.upperOverlayDirs) > 0:
				for i := range d.upperOverlayDirs {
					dir := d.upperOverlayDirs[len(d.upperOverlayDirs)-1-i]
					path, err := safeJoin(dir, c.subpath)
					if err != nil {
						return errors.Wrapf(err, "failed to join %s and %s", dir, c.subpath)
					}
					if stat, err := os.Lstat(path); err == nil {
						c.srcpath = path
						srcfi = stat
						break
					} else if errors.Is(err, unix.ENOENT) {
						continue
					} else {
						return errors.Wrap(err, "failed to lstat when finding direct path of overlay file")
					}
				}
			default:
				srcpath, err := safeJoin(d.upperRoot, subpath)
				if err != nil {
					return errors.Wrapf(err, "failed to join %s and %s", d.upperRoot, subpath)
				}
				c.srcpath = srcpath
				if fi, err := os.Lstat(c.srcpath); err == nil {
					srcfi = fi
				} else {
					return errors.Wrap(err, "failed to stat srcpath from differ")
				}
			}

			var ok bool
			c.srcStat, ok = srcfi.Sys().(*syscall.Stat_t)
			if !ok {
				return errors.Errorf("unhandled stat type for %+v", srcfi)
			}

			if !srcfi.IsDir() && c.srcStat.Nlink > 1 {
				if linkSubpath, ok := d.inodes[statInode(c.srcStat)]; ok {
					c.linkSubpath = linkSubpath
				} else {
					d.inodes[statInode(c.srcStat)] = c.subpath
				}
			}
		}

		return handle(ctx, c)
	})
}

func (d *differ) overlayChanges(ctx context.Context, handle func(context.Context, *change) error) error {
	return overlay.Changes(ctx, func(kind fs.ChangeKind, subpath string, srcfi os.FileInfo, prevErr error) error {
		if prevErr != nil {
			return prevErr
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		if kind == fs.ChangeKindUnmodified {
			return nil
		}

		if err := d.checkParent(ctx, subpath, handle); err != nil {
			return errors.Wrapf(err, "failed to check parent for %s", subpath)
		}

		srcpath, err := safeJoin(d.upperdir, subpath)
		if err != nil {
			return errors.Wrapf(err, "failed to join %s and %s", d.upperdir, subpath)
		}

		c := &change{
			kind:    kind,
			subpath: subpath,
			srcpath: srcpath,
		}

		if srcfi != nil {
			var ok bool
			c.srcStat, ok = srcfi.Sys().(*syscall.Stat_t)
			if !ok {
				return errors.Errorf("unhandled stat type for %+v", srcfi)
			}

			if !srcfi.IsDir() && c.srcStat.Nlink > 1 {
				if linkSubpath, ok := d.inodes[statInode(c.srcStat)]; ok {
					c.linkSubpath = linkSubpath
				} else {
					d.inodes[statInode(c.srcStat)] = c.subpath
				}
			}
		}

		return handle(ctx, c)
	}, d.upperdir, d.upperRoot, d.lowerRoot)
}

func (d *differ) checkParent(ctx context.Context, subpath string, handle func(context.Context, *change) error) error {
	parentSubpath := filepath.Dir(subpath)
	if parentSubpath == "/" {
		return nil
	}
	if _, ok := d.visited[parentSubpath]; ok {
		return nil
	}
	d.visited[parentSubpath] = struct{}{}

	if err := d.checkParent(ctx, parentSubpath, handle); err != nil {
		return err
	}
	parentSrcpath, err := safeJoin(d.upperRoot, parentSubpath)
	if err != nil {
		return err
	}
	srcfi, err := os.Lstat(parentSrcpath)
	if err != nil {
		return err
	}
	parentSrcStat, ok := srcfi.Sys().(*syscall.Stat_t)
	if !ok {
		return errors.Errorf("unexpected type %T", srcfi)
	}
	return handle(ctx, &change{
		kind:    fs.ChangeKindModify,
		subpath: parentSubpath,
		srcpath: parentSrcpath,
		srcStat: parentSrcStat,
	})
}

func (d *differ) Release() error {
	var err error
	if d.releaseLower != nil {
		err = d.releaseLower()
		if err == nil {
			d.releaseLower = nil
		}
	}
	if d.releaseUpper != nil {
		err = multierror.Append(err, d.releaseUpper()).ErrorOrNil()
		if err == nil {
			d.releaseUpper = nil
		}
	}
	return err
}

func safeJoin(root, path string) (string, error) {
	dir, base := filepath.Split(path)
	parent, err := fs.RootPath(root, dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(parent, base), nil
}

func isOpaqueXattr(s string) bool {
	for _, k := range []string{"trusted.overlay.opaque", "user.overlay.opaque"} {
		if s == k {
			return true
		}
	}
	return false
}

// needsUserXAttr checks whether overlay mounts should be provided the userxattr option. We can't use
// NeedsUserXAttr from the overlayutils package directly because we don't always have direct knowledge
// of the root of the snapshotter state (such as when using a remote snapshotter). Instead, we create
// a temporary new snapshot and test using its root, which works because single layer snapshots will
// use bind-mounts even when created by an overlay based snapshotter.
func needsUserXAttr(ctx context.Context, sn Snapshotter, lm leases.Manager) (bool, error) {
	key := identity.NewID()

	ctx, done, err := leaseutil.WithLease(ctx, lm, leaseutil.MakeTemporary)
	if err != nil {
		return false, errors.Wrap(err, "failed to create lease for checking user xattr")
	}
	defer done(context.TODO())

	err = sn.Prepare(ctx, key, "")
	if err != nil {
		return false, err
	}
	mntable, err := sn.Mounts(ctx, key)
	if err != nil {
		return false, err
	}
	mnts, unmount, err := mntable.Mount()
	if err != nil {
		return false, err
	}
	defer unmount()

	var userxattr bool
	if err := mount.WithTempMount(ctx, mnts, func(root string) error {
		var err error
		userxattr, err = overlayutils.NeedsUserXAttr(root)
		return err
	}); err != nil {
		return false, err
	}
	return userxattr, nil
}
