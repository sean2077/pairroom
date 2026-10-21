package projectsetup

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"

	"github.com/sean2077/pairroom/internal/relayclient/skill"
)

type plannedFile struct {
	path   string
	before fileState
	after  []byte
}

func makeSkillPlan(dir *os.Root, root, manifest string) plan {
	p := plan{item: Item{Component: "skill", Path: manifest, State: "blocked"}}
	sum := sha256.New()
	current := true
	var entry fileState
	for _, file := range skill.Files() {
		path := filepath.ToSlash(filepath.Join(filepath.Dir(manifest), filepath.FromSlash(file.Path)))
		p.item.Files = append(p.item.Files, path)
		if reason := protectedReason(root, path); reason != "" {
			p.item.Reason = reason
			return p
		}
		before, err := readFile(dir, path)
		if err != nil {
			p.item.Reason = "unsafe_path"
			return p
		}
		after := []byte(file.Content)
		// Pin both the inspected files and the proposed bundle. Restarting on a
		// newer binary must not reuse consent for different reference contents.
		fmt.Fprintf(sum, "%s:%x\n", revision(path, before), sha256.Sum256(after))
		p.files = append(p.files, plannedFile{path: path, before: before, after: after})
		current = current && before.exists && bytes.Equal(before.data, after)
		if file.Path == "SKILL.md" {
			entry = before
		}
	}
	p.item.Revision = hex.EncodeToString(sum.Sum(nil))
	switch {
	case current:
		p.item.State = "current"
	case entry.exists || entry.parentExists:
		p.item.State, p.item.Reason = "conflict", "existing_skill"
		p.item.CanInstall, p.item.NeedsConfirmation = true, true
		if bytes.Contains(entry.data, []byte("name: pairroom-relay")) && bytes.Contains(entry.data, []byte("# pairroom-relay")) {
			p.item.State = "update_required"
		}
	default:
		p.item.State, p.item.CanInstall = "missing", true
	}
	return p
}

type stagedSkillFile struct {
	plannedFile
	parent    *os.Root
	temporary string
}

// replaceSkillFiles validates the whole preview before creating directories or
// staging replacements. Every changed file is staged and rechecked before any
// publication; references are published before the manifest. A filesystem error
// during publication may leave a partial bundle, which Inspect reports as stale
// and requires a new explicit confirmation to repair. This is not a directory
// transaction or an external-editor lock.
func replaceSkillFiles(root *os.Root, files []plannedFile) error {
	for _, file := range files {
		current, err := readFile(root, file.path)
		if err != nil {
			return err
		}
		if revision(file.path, current) != revision(file.path, file.before) {
			return ErrChanged
		}
	}
	var staged []stagedSkillFile
	defer func() {
		for _, file := range staged {
			file.parent.Remove(file.temporary)
			file.parent.Close()
		}
	}()
	// The embedded manifest sorts first, but must be published last.
	ordered := make([]plannedFile, 0, len(files))
	for _, file := range files {
		if filepath.Base(file.path) != "SKILL.md" {
			ordered = append(ordered, file)
		}
	}
	for _, file := range files {
		if filepath.Base(file.path) == "SKILL.md" {
			ordered = append(ordered, file)
		}
	}
	for _, file := range ordered {
		if file.before.exists && bytes.Equal(file.before.data, file.after) {
			continue
		}
		parent, err := openParent(root, file.path, true)
		if err != nil {
			return err
		}
		var random [16]byte
		if _, err := rand.Read(random[:]); err != nil {
			parent.Close()
			return err
		}
		temporary := ".pairroom-setup-" + hex.EncodeToString(random[:])
		pending := stagedSkillFile{plannedFile: file, parent: parent, temporary: temporary}
		staged = append(staged, pending)
		output, err := parent.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, file.before.mode)
		if err != nil {
			return err
		}
		if err = output.Chmod(file.before.mode); err == nil {
			_, err = output.Write(file.after)
		}
		if err == nil {
			err = output.Sync()
		}
		closeErr := output.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	for _, file := range files {
		current, err := readFile(root, file.path)
		if err != nil {
			return err
		}
		// Staging may have created a previously missing parent, but it has not
		// published a managed file. All other preview facts must still match.
		current.parentExists = file.before.parentExists
		if revision(file.path, current) != revision(file.path, file.before) {
			return ErrChanged
		}
	}
	for _, file := range staged {
		name := filepath.Base(file.path)
		current, err := readLeaf(file.parent, name)
		if err != nil {
			return err
		}
		current.parentExists = file.before.parentExists
		if revision(file.path, current) != revision(file.path, file.before) {
			return ErrChanged
		}
		if file.before.exists {
			err = file.parent.Rename(file.temporary, name)
		} else {
			err = file.parent.Link(file.temporary, name)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
