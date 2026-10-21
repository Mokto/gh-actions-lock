package resolve

import (
	"strings"

	parserlock "github.com/github/actions-lockfile/go/pkg/lockfile"
	"github.com/github/gh-actions-lock/internal/ghapi"
	"github.com/github/gh-actions-lock/internal/uses"
	"gopkg.in/yaml.v3"
)

// ManifestImages returns the unpinned images the action manifest yml runs:
// a Docker action's `runs.image` (a registry image or a Dockerfile it
// builds), or a composite's `docker://` steps. action names it as
// owner/repo[/path]@ref.
func ManifestImages(yml, action string) []uses.Image {
	meta, err := parserlock.ParseActionMeta(yml)
	if err != nil {
		return nil
	}
	var out []uses.Image
	add := func(img uses.Image, ok bool) {
		if ok {
			img.Where, img.Action = action, action
			out = append(out, img)
		}
	}
	switch meta.Execution {
	case parserlock.ExecDocker:
		var m struct {
			Runs struct {
				Image string `yaml:"image"`
			} `yaml:"runs"`
		}
		if yaml.Unmarshal([]byte(yml), &m) != nil {
			return nil
		}
		image := strings.TrimSpace(m.Runs.Image)
		switch {
		case strings.HasPrefix(image, "docker://"):
			add(uses.ClassifyImage(image))
		case image != "":
			add(uses.Image{Name: image, Kind: uses.ImageBuild}, true)
		}
	case parserlock.ExecComposite:
		for _, use := range meta.NestedUses {
			if strings.HasPrefix(strings.TrimSpace(use), "docker://") {
				add(uses.ClassifyImage(use))
			}
		}
	}
	return out
}

// ContainerImages returns the unpinned images that refs and the composites
// they reach run, from manifests this resolver already read. Entries seeded
// from the lockfile carry no manifest and are skipped.
func (r *Resolver) ContainerImages(refs []parserlock.ActionRef) []uses.Image {
	pending := make([]NestedUse, len(refs))
	for i, ref := range refs {
		pending[i] = NestedUse{Ref: ref}
	}
	seen := map[ghapi.ActionRef]bool{}
	var out []uses.Image
	for len(pending) > 0 {
		ref := pending[0].Ref
		pending = pending[1:]
		key := cacheKey(ref)
		if seen[key] {
			continue
		}
		seen[key] = true
		entry, ok := r.cache.Get(key)
		if !ok || entry.actionYML == "" {
			continue
		}
		out = append(out, ManifestImages(entry.actionYML, ref.FullName()+"@"+ref.Ref)...)
		nested, _ := CompositeUses(entry.dep, entry.actionYML)
		pending = append(pending, nested...)
	}
	return out
}
