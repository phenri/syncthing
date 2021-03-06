package model

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/calmh/syncthing/buffers"
	"github.com/calmh/syncthing/cid"
	"github.com/calmh/syncthing/config"
	"github.com/calmh/syncthing/osutil"
	"github.com/calmh/syncthing/protocol"
	"github.com/calmh/syncthing/scanner"
	"github.com/calmh/syncthing/versioner"
)

type requestResult struct {
	node     string
	file     scanner.File
	filepath string // full filepath name
	offset   int64
	data     []byte
	err      error
}

type openFile struct {
	filepath     string // full filepath name
	temp         string // temporary filename
	availability uint64 // availability bitset
	file         *os.File
	err          error // error when opening or writing to file, all following operations are cancelled
	outstanding  int   // number of requests we still have outstanding
	done         bool  // we have sent all requests for this file
}

type activityMap map[string]int

func (m activityMap) leastBusyNode(availability uint64, cm *cid.Map) string {
	var low int = 2<<30 - 1
	var selected string
	for _, node := range cm.Names() {
		id := cm.Get(node)
		if id == cid.LocalID {
			continue
		}
		usage := m[node]
		if availability&(1<<id) != 0 {
			if usage < low {
				low = usage
				selected = node
			}
		}
	}
	m[selected]++
	return selected
}

func (m activityMap) decrease(node string) {
	m[node]--
}

var errNoNode = errors.New("no available source node")

type puller struct {
	cfg               *config.Configuration
	repoCfg           config.RepositoryConfiguration
	bq                *blockQueue
	model             *Model
	oustandingPerNode activityMap
	openFiles         map[string]openFile
	requestSlots      chan bool
	blocks            chan bqBlock
	requestResults    chan requestResult
	versioner         versioner.Versioner
}

func newPuller(repoCfg config.RepositoryConfiguration, model *Model, slots int, cfg *config.Configuration) *puller {
	p := &puller{
		repoCfg:           repoCfg,
		cfg:               cfg,
		bq:                newBlockQueue(),
		model:             model,
		oustandingPerNode: make(activityMap),
		openFiles:         make(map[string]openFile),
		requestSlots:      make(chan bool, slots),
		blocks:            make(chan bqBlock),
		requestResults:    make(chan requestResult),
	}

	if len(repoCfg.Versioning.Type) > 0 {
		factory, ok := versioner.Factories[repoCfg.Versioning.Type]
		if !ok {
			l.Fatalf("Requested versioning type %q that does not exist", repoCfg.Versioning.Type)
		}
		p.versioner = factory(repoCfg.Versioning.Params)
	}

	if slots > 0 {
		// Read/write
		for i := 0; i < slots; i++ {
			p.requestSlots <- true
		}
		if debug {
			l.Debugf("starting puller; repo %q dir %q slots %d", repoCfg.ID, repoCfg.Directory, slots)
		}
		go p.run()
	} else {
		// Read only
		if debug {
			l.Debugf("starting puller; repo %q dir %q (read only)", repoCfg.ID, repoCfg.Directory)
		}
		go p.runRO()
	}
	return p
}

func (p *puller) run() {
	go func() {
		// fill blocks queue when there are free slots
		for {
			<-p.requestSlots
			b := p.bq.get()
			if debug {
				l.Debugf("filler: queueing %q / %q offset %d copy %d", p.repoCfg.ID, b.file.Name, b.block.Offset, len(b.copy))
			}
			p.blocks <- b
		}
	}()

	walkTicker := time.Tick(time.Duration(p.cfg.Options.RescanIntervalS) * time.Second)
	timeout := time.Tick(5 * time.Second)
	changed := true

	for {
		// Run the pulling loop as long as there are blocks to fetch
	pull:
		for {
			select {
			case res := <-p.requestResults:
				p.model.setState(p.repoCfg.ID, RepoSyncing)
				changed = true
				p.requestSlots <- true
				p.handleRequestResult(res)

			case b := <-p.blocks:
				p.model.setState(p.repoCfg.ID, RepoSyncing)
				changed = true
				if p.handleBlock(b) {
					// Block was fully handled, free up the slot
					p.requestSlots <- true
				}

			case <-timeout:
				if len(p.openFiles) == 0 && p.bq.empty() {
					// Nothing more to do for the moment
					break pull
				}
				if debug {
					l.Debugf("%q: idle but have %d open files", p.repoCfg.ID, len(p.openFiles))
					i := 5
					for _, f := range p.openFiles {
						l.Debugf("  %v", f)
						i--
						if i == 0 {
							break
						}
					}
				}
			}
		}

		if changed {
			p.model.setState(p.repoCfg.ID, RepoCleaning)
			p.fixupDirectories()
			changed = false
		}

		p.model.setState(p.repoCfg.ID, RepoIdle)

		// Do a rescan if it's time for it
		select {
		case <-walkTicker:
			if debug {
				l.Debugf("%q: time for rescan", p.repoCfg.ID)
			}
			err := p.model.ScanRepo(p.repoCfg.ID)
			if err != nil {
				invalidateRepo(p.cfg, p.repoCfg.ID, err)
				return
			}

		default:
		}

		// Queue more blocks to fetch, if any
		p.queueNeededBlocks()
	}
}

func (p *puller) runRO() {
	walkTicker := time.Tick(time.Duration(p.cfg.Options.RescanIntervalS) * time.Second)

	for _ = range walkTicker {
		if debug {
			l.Debugf("%q: time for rescan", p.repoCfg.ID)
		}
		err := p.model.ScanRepo(p.repoCfg.ID)
		if err != nil {
			invalidateRepo(p.cfg, p.repoCfg.ID, err)
			return
		}
	}
}

func (p *puller) fixupDirectories() {
	var deleteDirs []string
	var changed = 0

	var walkFn = func(path string, info os.FileInfo, err error) error {
		if !info.IsDir() {
			return nil
		}

		rn, err := filepath.Rel(p.repoCfg.Directory, path)
		if err != nil {
			return nil
		}

		if rn == "." {
			return nil
		}

		if filepath.Base(rn) == ".stversions" {
			return nil
		}

		cur := p.model.CurrentRepoFile(p.repoCfg.ID, rn)
		if cur.Name != rn {
			// No matching dir in current list; weird
			if debug {
				l.Debugf("missing dir: %s; %v", rn, cur)
			}
			return nil
		}

		if protocol.IsDeleted(cur.Flags) {
			if debug {
				l.Debugf("queue delete dir: %v", cur)
			}

			// We queue the directories to delete since we walk the
			// tree in depth first order and need to remove the
			// directories in the opposite order.

			deleteDirs = append(deleteDirs, path)
			return nil
		}

		if !p.repoCfg.IgnorePerms && protocol.HasPermissionBits(cur.Flags) && !scanner.PermsEqual(cur.Flags, uint32(info.Mode())) {
			err := os.Chmod(path, os.FileMode(cur.Flags)&os.ModePerm)
			if err != nil {
				l.Warnf("Restoring folder flags: %q: %v", path, err)
			} else {
				changed++
				if debug {
					l.Debugf("restored dir flags: %o -> %v", info.Mode()&os.ModePerm, cur)
				}
			}
		}

		if cur.Modified != info.ModTime().Unix() {
			t := time.Unix(cur.Modified, 0)
			err := os.Chtimes(path, t, t)
			if err != nil {
				l.Warnf("Restoring folder modtime: %q: %v", path, err)
			} else {
				changed++
				if debug {
					l.Debugf("restored dir modtime: %d -> %v", info.ModTime().Unix(), cur)
				}
			}
		}

		return nil
	}

	for {
		deleteDirs = nil
		changed = 0
		filepath.Walk(p.repoCfg.Directory, walkFn)

		var deleted = 0
		// Delete any queued directories
		for i := len(deleteDirs) - 1; i >= 0; i-- {
			dir := deleteDirs[i]
			if debug {
				l.Debugln("delete dir:", dir)
			}
			err := os.Remove(dir)
			if err == nil {
				deleted++
			} else if p.versioner == nil { // Failures are expected in the presence of versioning
				l.Warnln(err)
			}
		}

		if debug {
			l.Debugf("changed %d, deleted %d dirs", changed, deleted)
		}

		if changed+deleted == 0 {
			return
		}
	}
}

func (p *puller) handleRequestResult(res requestResult) {
	p.oustandingPerNode.decrease(res.node)
	f := res.file

	of, ok := p.openFiles[f.Name]
	if !ok || of.err != nil {
		// no entry in openFiles means there was an error and we've cancelled the operation
		return
	}

	_, of.err = of.file.WriteAt(res.data, res.offset)
	buffers.Put(res.data)

	of.outstanding--
	p.openFiles[f.Name] = of

	if debug {
		l.Debugf("pull: wrote %q / %q offset %d outstanding %d done %v", p.repoCfg.ID, f.Name, res.offset, of.outstanding, of.done)
	}

	if of.done && of.outstanding == 0 {
		p.closeFile(f)
	}
}

// handleBlock fulfills the block request by copying, ignoring or fetching
// from the network. Returns true if the block was fully handled
// synchronously, i.e. if the slot can be reused.
func (p *puller) handleBlock(b bqBlock) bool {
	f := b.file

	// For directories, making sure they exist is enough.
	// Deleted directories we mark as handled and delete later.
	if protocol.IsDirectory(f.Flags) {
		if !protocol.IsDeleted(f.Flags) {
			path := filepath.Join(p.repoCfg.Directory, f.Name)
			_, err := os.Stat(path)
			if err != nil && os.IsNotExist(err) {
				if debug {
					l.Debugf("create dir: %v", f)
				}
				err = os.MkdirAll(path, 0777)
				if err != nil {
					l.Warnf("Create folder: %q: %v", path, err)
				}
			}
		} else if debug {
			l.Debugf("ignore delete dir: %v", f)
		}
		p.model.updateLocal(p.repoCfg.ID, f)
		return true
	}

	of, ok := p.openFiles[f.Name]
	of.done = b.last

	if !ok {
		if debug {
			l.Debugf("pull: %q: opening file %q", p.repoCfg.ID, f.Name)
		}

		of.availability = uint64(p.model.repoFiles[p.repoCfg.ID].Availability(f.Name))
		of.filepath = filepath.Join(p.repoCfg.Directory, f.Name)
		of.temp = filepath.Join(p.repoCfg.Directory, defTempNamer.TempName(f.Name))

		dirName := filepath.Dir(of.filepath)
		_, err := os.Stat(dirName)
		if err != nil {
			err = os.MkdirAll(dirName, 0777)
		}
		if err != nil {
			l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, err)
		}

		of.file, of.err = os.Create(of.temp)
		if of.err != nil {
			if debug {
				l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, of.err)
			}
			if !b.last {
				p.openFiles[f.Name] = of
			}
			return true
		}
		osutil.HideFile(of.temp)
	}

	if of.err != nil {
		// We have already failed this file.
		if debug {
			l.Debugf("pull: error: %q / %q has already failed: %v", p.repoCfg.ID, f.Name, of.err)
		}
		if b.last {
			delete(p.openFiles, f.Name)
		}

		return true
	}

	p.openFiles[f.Name] = of

	switch {
	case len(b.copy) > 0:
		p.handleCopyBlock(b)
		return true

	case b.block.Size > 0:
		return p.handleRequestBlock(b)

	default:
		p.handleEmptyBlock(b)
		return true
	}
}

func (p *puller) handleCopyBlock(b bqBlock) {
	// We have blocks to copy from the existing file
	f := b.file
	of := p.openFiles[f.Name]

	if debug {
		l.Debugf("pull: copying %d blocks for %q / %q", len(b.copy), p.repoCfg.ID, f.Name)
	}

	var exfd *os.File
	exfd, of.err = os.Open(of.filepath)
	if of.err != nil {
		if debug {
			l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, of.err)
		}
		of.file.Close()
		of.file = nil

		p.openFiles[f.Name] = of
		return
	}
	defer exfd.Close()

	for _, b := range b.copy {
		bs := buffers.Get(int(b.Size))
		_, of.err = exfd.ReadAt(bs, b.Offset)
		if of.err == nil {
			_, of.err = of.file.WriteAt(bs, b.Offset)
		}
		buffers.Put(bs)
		if of.err != nil {
			if debug {
				l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, of.err)
			}
			exfd.Close()
			of.file.Close()
			of.file = nil

			p.openFiles[f.Name] = of
			return
		}
	}
}

// handleRequestBlock tries to pull a block from the network. Returns true if
// the block could _not_ be fetched (i.e. it was fully handled, matching the
// return criteria of handleBlock)
func (p *puller) handleRequestBlock(b bqBlock) bool {
	f := b.file
	of, ok := p.openFiles[f.Name]
	if !ok {
		panic("bug: request for non-open file")
	}

	node := p.oustandingPerNode.leastBusyNode(of.availability, p.model.cm)
	if len(node) == 0 {
		of.err = errNoNode
		if of.file != nil {
			of.file.Close()
			of.file = nil
			os.Remove(of.temp)
		}
		if b.last {
			delete(p.openFiles, f.Name)
		} else {
			p.openFiles[f.Name] = of
		}
		return true
	}

	of.outstanding++
	p.openFiles[f.Name] = of

	go func(node string, b bqBlock) {
		if debug {
			l.Debugf("pull: requesting %q / %q offset %d size %d from %q outstanding %d", p.repoCfg.ID, f.Name, b.block.Offset, b.block.Size, node, of.outstanding)
		}

		bs, err := p.model.requestGlobal(node, p.repoCfg.ID, f.Name, b.block.Offset, int(b.block.Size), nil)
		p.requestResults <- requestResult{
			node:     node,
			file:     f,
			filepath: of.filepath,
			offset:   b.block.Offset,
			data:     bs,
			err:      err,
		}
	}(node, b)

	return false
}

func (p *puller) handleEmptyBlock(b bqBlock) {
	f := b.file
	of := p.openFiles[f.Name]

	if b.last {
		if of.err == nil {
			of.file.Close()
		}
	}

	if protocol.IsDeleted(f.Flags) {
		if debug {
			l.Debugf("pull: delete %q", f.Name)
		}
		os.Remove(of.temp)
		os.Chmod(of.filepath, 0666)
		if p.versioner != nil {
			if err := p.versioner.Archive(of.filepath); err == nil {
				p.model.updateLocal(p.repoCfg.ID, f)
			}
		} else if err := os.Remove(of.filepath); err == nil || os.IsNotExist(err) {
			p.model.updateLocal(p.repoCfg.ID, f)
		}
	} else {
		if debug {
			l.Debugf("pull: no blocks to fetch and nothing to copy for %q / %q", p.repoCfg.ID, f.Name)
		}
		t := time.Unix(f.Modified, 0)
		if os.Chtimes(of.temp, t, t) != nil {
			delete(p.openFiles, f.Name)
			return
		}
		if !p.repoCfg.IgnorePerms && protocol.HasPermissionBits(f.Flags) && os.Chmod(of.temp, os.FileMode(f.Flags&0777)) != nil {
			delete(p.openFiles, f.Name)
			return
		}
		osutil.ShowFile(of.temp)
		if osutil.Rename(of.temp, of.filepath) == nil {
			p.model.updateLocal(p.repoCfg.ID, f)
		}
	}
	delete(p.openFiles, f.Name)
}

func (p *puller) queueNeededBlocks() {
	queued := 0
	for _, f := range p.model.NeedFilesRepo(p.repoCfg.ID) {
		lf := p.model.CurrentRepoFile(p.repoCfg.ID, f.Name)
		have, need := scanner.BlockDiff(lf.Blocks, f.Blocks)
		if debug {
			l.Debugf("need:\n  local: %v\n  global: %v\n  haveBlocks: %v\n  needBlocks: %v", lf, f, have, need)
		}
		queued++
		p.bq.put(bqAdd{
			file: f,
			have: have,
			need: need,
		})
	}
	if debug && queued > 0 {
		l.Debugf("%q: queued %d blocks", p.repoCfg.ID, queued)
	}
}

func (p *puller) closeFile(f scanner.File) {
	if debug {
		l.Debugf("pull: closing %q / %q", p.repoCfg.ID, f.Name)
	}

	of := p.openFiles[f.Name]
	of.file.Close()
	defer os.Remove(of.temp)

	delete(p.openFiles, f.Name)

	fd, err := os.Open(of.temp)
	if err != nil {
		if debug {
			l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, err)
		}
		return
	}
	hb, _ := scanner.Blocks(fd, scanner.StandardBlockSize)
	fd.Close()

	if l0, l1 := len(hb), len(f.Blocks); l0 != l1 {
		if debug {
			l.Debugf("pull: %q / %q: nblocks %d != %d", p.repoCfg.ID, f.Name, l0, l1)
		}
		return
	}

	for i := range hb {
		if bytes.Compare(hb[i].Hash, f.Blocks[i].Hash) != 0 {
			l.Debugf("pull: %q / %q: block %d hash mismatch", p.repoCfg.ID, f.Name, i)
			return
		}
	}

	t := time.Unix(f.Modified, 0)
	err = os.Chtimes(of.temp, t, t)
	if debug && err != nil {
		l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, err)
	}
	if !p.repoCfg.IgnorePerms && protocol.HasPermissionBits(f.Flags) {
		err = os.Chmod(of.temp, os.FileMode(f.Flags&0777))
		if debug && err != nil {
			l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, err)
		}
	}

	osutil.ShowFile(of.temp)

	if p.versioner != nil {
		err := p.versioner.Archive(of.filepath)
		if err != nil {
			if debug {
				l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, err)
			}
			return
		}
	}

	if debug {
		l.Debugf("pull: rename %q / %q: %q", p.repoCfg.ID, f.Name, of.filepath)
	}
	if err := osutil.Rename(of.temp, of.filepath); err == nil {
		p.model.updateLocal(p.repoCfg.ID, f)
	} else {
		l.Debugf("pull: error: %q / %q: %v", p.repoCfg.ID, f.Name, err)
	}
}

func invalidateRepo(cfg *config.Configuration, repoID string, err error) {
	for i := range cfg.Repositories {
		repo := &cfg.Repositories[i]
		if repo.ID == repoID {
			repo.Invalid = err.Error()
			return
		}
	}
}
