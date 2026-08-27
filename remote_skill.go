package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"

	"errors"
	"fmt"

	"io/fs"

	"time"
	"unicode"
)

const (
	remoteSkillSchemaRevision         = 1
	remoteSkillOverrideSchemaRevision = 1
	remoteSkillCacheTTL               = 3 * time.Hour
	remoteSkillMaxFiles               = 1024
	remoteSkillMaxBytes               = 16 << 20
	remoteSkillMetadataLimit          = 64 << 10
	remoteContentGracePeriod          = 2 * remoteSkillCacheTTL
	remoteSkillPatchDir               = ".remote-patches"
	skillsShProvider                  = "skills.sh"
	skillsMPProvider                  = "SkillsMP"
)

var errRemoteSkillPatch = errors.New("remote skill patch no longer applies")
var errRemoteSkillEditConflict = errors.New("remote skill changed while it was being edited")

const (
	remoteSkillPatchBaseHeader   = "# skills-mgr-base-sha256 "
	remoteSkillPatchResultHeader = "# skills-mgr-result-sha256 "
)

type remoteSkillRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
	Name     string `json:"name"`
	Locator  string `json:"locator"`
}

func (r remoteSkillRef) key() string {
	sum := sha256.Sum256([]byte(r.Provider + "\x00" + r.ID))
	return hex.EncodeToString(sum[:])
}

func (r remoteSkillRef) validate() error {
	if r.Provider != skillsShProvider && r.Provider != skillsMPProvider {
		return fmt.Errorf("unsupported remote skill provider %q", r.Provider)
	}
	if r.ID == "" || len(r.ID) > 1024 {
		return fmt.Errorf("invalid %s skill identifier", r.Provider)
	}
	if !validSkillName(r.Name) {
		return fmt.Errorf("invalid remote skill name %q", r.Name)
	}
	if r.Locator == "" || len(r.Locator) > 4096 {
		return fmt.Errorf("invalid remote skill locator for %q", r.Name)
	}
	for _, character := range r.ID + r.Locator {
		if unicode.IsControl(character) {
			return fmt.Errorf("invalid remote skill metadata for %q", r.Name)
		}
	}
	return nil
}

type remoteSkillFile struct {
	Path     string
	Contents []byte
	Mode     fs.FileMode
}

type remoteSkillContentProvider interface {
	fetchSkill(
		ctx context.Context,
		ref remoteSkillRef,
	) ([]remoteSkillFile, error)
}

type remoteSkillRecord struct {
	SchemaRevision int       `json:"schemaRevision"`
	Provider       string    `json:"provider"`
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Locator        string    `json:"locator"`
	FetchedAt      time.Time `json:"fetchedAt"`
	Content        string    `json:"content"`
	// Kept outside the fetched record so refreshes cannot overwrite local policy.
	disableModelInvocationOverride *bool
}

type remoteSkillOverride struct {
	SchemaRevision         int   `json:"schemaRevision"`
	DisableModelInvocation *bool `json:"disableModelInvocation"`
}

func (r remoteSkillRecord) ref() remoteSkillRef {
	return remoteSkillRef{
		Provider: r.Provider,
		ID:       r.ID,
		Name:     r.Name,
		Locator:  r.Locator,
	}
}

func (r remoteSkillRecord) fresh(now time.Time) bool {
	return now.Before(r.FetchedAt.Add(remoteSkillCacheTTL))
}
