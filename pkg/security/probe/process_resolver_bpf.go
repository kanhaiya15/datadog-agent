// Unless explicitly stated otherwise all files in this repository are licensed
// under the Apache License Version 2.0.
// This product includes software developed at Datadog (https://www.datadoghq.com/).
// Copyright 2016-2020 Datadog, Inc.

// +build linux_bpf

package probe

import (
	"os"
	"syscall"

	"github.com/pkg/errors"

	"github.com/DataDog/datadog-agent/pkg/security/ebpf"
	"github.com/DataDog/datadog-agent/pkg/security/utils"
	"github.com/DataDog/datadog-agent/pkg/util/log"
	"github.com/DataDog/gopsutil/process"
)

// processSnapshotTables list of tables used to snapshot
var processSnapshotTables = []string{
	"inode_numlower",
}

// processSnapshotProbes list of hooks used to snapshot
var processSnapshotProbes = []*ebpf.KProbe{
	{
		Name:      "getattr",
		EntryFunc: "kprobe/vfs_getattr",
	},
}

// ProcessResolver resolved process context
type ProcessResolver struct {
	probe            *Probe
	resolvers        *Resolvers
	inodeNumlowerMap *ebpf.Table
	procCacheMap     *ebpf.Table
	pidCookieMap     *ebpf.Table
	entryCache       map[uint32]*ProcessResolverEntry
}

// AddEntry add an entry to the local cache
func (p *ProcessResolver) AddEntry(pid uint32, entry *ProcessResolverEntry) {
	p.entryCache[pid] = entry
}

// DelEntry removes an entry from the cache
func (p *ProcessResolver) DelEntry(pid uint32) {
	delete(p.entryCache, pid)

	pidb := make([]byte, 4)
	byteOrder.PutUint32(pidb, pid)

	p.pidCookieMap.Delete(pidb)
}

func (p *ProcessResolver) resolve(pid uint32) *ProcessResolverEntry {
	pidb := make([]byte, 4)
	byteOrder.PutUint32(pidb, pid)

	cookieb, err := p.pidCookieMap.Get(pidb)
	if err != nil {
		return nil
	}

	entryb, err := p.procCacheMap.Get(cookieb)
	if err != nil {
		return nil
	}

	var procCacheEntry ProcCacheEntry
	if _, err := procCacheEntry.UnmarshalBinary(entryb); err != nil {
		return nil
	}

	pathnameStr := procCacheEntry.FileEvent.ResolveInode(p.resolvers)
	if pathnameStr == dentryPathKeyNotFound {
		return nil
	}

	timestamp := p.resolvers.TimeResolver.ResolveMonotonicTimestamp(procCacheEntry.TimestampRaw)

	entry := &ProcessResolverEntry{
		PathnameStr: pathnameStr,
		Timestamp:   timestamp,
	}
	p.AddEntry(pid, entry)

	return entry
}

// Resolve returns the cache entry for the given pid
func (p *ProcessResolver) Resolve(pid uint32) *ProcessResolverEntry {
	entry, ok := p.entryCache[pid]
	if ok {
		return entry
	}

	// fallback request the map directly, the perf event should be delayed
	return p.resolve(pid)
}

// Start starts the resolver
func (p *ProcessResolver) Start() error {
	// Select the in-kernel process cache that will be populated by the snapshot
	p.procCacheMap = p.probe.Table("proc_cache")
	if p.procCacheMap == nil {
		return errors.New("proc_cache BPF_HASH table doesn't exist")
	}

	// Select the in-kernel pid <-> cookie cache
	p.pidCookieMap = p.probe.Table("pid_cookie")
	if p.pidCookieMap == nil {
		return errors.New("pid_cookie BPF_HASH table doesn't exist")
	}

	return nil
}

func (p *ProcessResolver) snapshot() error {
	processes, err := process.AllProcesses()
	if err != nil {
		return err
	}

	cacheModified := false

	for _, proc := range processes {
		// If Exe is not set, the process is a short lived process and its /proc entry has already expired, move on.
		if len(proc.Exe) == 0 {
			continue
		}

		// Notify that we modified the cache.
		if p.snapshotProcess(uint32(proc.Pid)) {
			cacheModified = true
		}
	}

	// There is a possible race condition where a process could have started right after we did the call to
	// process.AllProcesses and before we inserted the cache entry of its parent. Call Snapshot again until we
	// do not modify the process cache anymore
	if cacheModified {
		return errors.New("cache modified")
	}

	return nil
}

// snapshotProcess snapshots /proc for the provided pid. This method returns true if it updated the kernel process cache.
func (p *ProcessResolver) snapshotProcess(pid uint32) bool {
	entry := ProcCacheEntry{}
	pidb := make([]byte, 4)
	cookieb := make([]byte, 4)
	inodeb := make([]byte, 8)

	// Check if there already is an entry in the pid <-> cookie cache
	byteOrder.PutUint32(pidb, pid)
	if _, err := p.pidCookieMap.Get(pidb); err == nil {
		// If there is a cookie, there is an entry in cache, we don't need to do anything else
		return false
	}

	// Populate the mount point cache for the process
	if err := p.resolvers.MountResolver.SyncCache(pid); err != nil {
		if !os.IsNotExist(err) {
			log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't sync mount points", pid))
			return false
		}
	}

	// Retrieve the container ID of the process
	containerID, err := p.resolvers.ContainerResolver.GetContainerID(pid)
	if err != nil {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't parse container ID", pid))
		return false
	}
	entry.ContainerEvent.ID = string(containerID)

	procExecPath := utils.ProcExePath(pid)

	// Get process filename and pre-fill the cache
	pathnameStr, err := os.Readlink(procExecPath)
	if err != nil {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't readlink binary", pid))
		return false
	}
	p.AddEntry(pid, &ProcessResolverEntry{
		PathnameStr: pathnameStr,
	})

	// Get the inode of the process binary
	fi, err := os.Stat(procExecPath)
	if err != nil {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't stat binary", pid))
		return false
	}
	stat, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't stat binary", pid))
		return false
	}
	entry.Inode = stat.Ino

	// Fetch the numlower value of the inode
	byteOrder.PutUint64(inodeb, stat.Ino)
	numlowerb, err := p.inodeNumlowerMap.Get(inodeb)
	if err != nil {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't retrieve numlower value", pid))
		return false
	}
	entry.OverlayNumLower = int32(byteOrder.Uint32(numlowerb))

	// Generate a new cookie for this pid
	byteOrder.PutUint32(cookieb, utils.NewCookie())

	// Insert the new cache entry and then the cookie
	if err := p.procCacheMap.SetP(cookieb, entry.Bytes()); err != nil {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't insert cache entry", pid))
		return false
	}
	if err := p.pidCookieMap.SetP(pidb, cookieb); err != nil {
		log.Debug(errors.Wrapf(err, "snapshot failed for %d: couldn't insert cookie", pid))
		return false
	}

	return true
}

// Snapshot retrieves the process informations
func (p *ProcessResolver) Snapshot() error {
	// Register snapshot tables
	for _, t := range processSnapshotTables {
		if err := p.probe.RegisterTable(t); err != nil {
			return err
		}
	}

	// Select the inode numlower map to prepare for the snapshot
	p.inodeNumlowerMap = p.probe.Table("inode_numlower")
	if p.inodeNumlowerMap == nil {
		return errors.New("inode_numlower BPF_HASH table doesn't exist")
	}

	// Activate the probes required by the snapshot
	for _, kp := range processSnapshotProbes {
		if err := p.probe.Module.RegisterKprobe(kp); err != nil {
			return errors.Wrapf(err, "couldn't register kprobe %s", kp.Name)
		}
	}

	// Deregister probes
	defer func() {
		for _, kp := range processSnapshotProbes {
			if err := p.probe.Module.UnregisterKprobe(kp); err != nil {
				log.Debugf("couldn't unregister kprobe %s: %v", kp.Name, err)
			}
		}
	}()

	for retry := 0; retry < 5; retry++ {
		if err := p.snapshot(); err == nil {
			return nil
		}
	}

	return errors.New("unable to snapshot processes")
}

// NewProcessResolver returns a new process resolver
func NewProcessResolver(probe *Probe, resolvers *Resolvers) (*ProcessResolver, error) {
	return &ProcessResolver{
		probe:      probe,
		resolvers:  resolvers,
		entryCache: make(map[uint32]*ProcessResolverEntry),
	}, nil
}
