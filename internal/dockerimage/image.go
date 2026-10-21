// Package dockerimage classifies the container images that workflows and
// actions run. The lockfile pins action commits; it can't pin these images.
//
// Classification is static and deliberately shallow: a reference is pinned
// when it names any `@sha256:` digest, whether or not the digest is well
// formed, and a reference built from an expression is not judged at all.
package dockerimage

import (
	"fmt"
	"strings"
)

// Kind is how an image reference is fixed, if at all.
type Kind int

const (
	// Digest is a reference with an `@sha256:` digest: immutable.
	Digest Kind = iota
	// Tagged is a reference by tag.
	Tagged
	// Untagged has neither tag nor digest; the runner pulls `latest`.
	Untagged
	// Built is a Docker action's Dockerfile, built on the runner each run.
	Built
	// Dynamic contains a `${{ }}` expression, known only at run time.
	Dynamic
	// Empty is `docker://` with no image name.
	Empty
)

const dockerPrefix = "docker://"

// IsDockerUses reports whether a `uses:` value runs a container image.
func IsDockerUses(uses string) bool {
	return strings.HasPrefix(strings.TrimSpace(uses), dockerPrefix)
}

// Classify reports the kind of a step's `docker://` value or a Docker
// action's `runs.image`. Anything other than `docker://` in `runs.image` is a
// Dockerfile path.
func Classify(image string) Kind {
	image = strings.TrimSpace(image)
	if strings.Contains(image, "${{") {
		return Dynamic
	}
	if !IsDockerUses(image) {
		return Built
	}
	return classifyName(strings.TrimPrefix(image, dockerPrefix))
}

// classifyName classifies a plain image name such as `ghcr.io/o/img:1`.
func classifyName(name string) Kind {
	name = strings.TrimSpace(name)
	switch {
	case strings.Contains(name, "${{"):
		return Dynamic
	case name == "":
		return Empty
	case strings.Contains(strings.ToLower(name), "@sha256:"):
		return Digest
	}
	// A registry port also uses ':', so only the last path element can hold the tag.
	if strings.Contains(name[strings.LastIndex(name, "/")+1:], ":") {
		return Tagged
	}
	return Untagged
}

// Image is a container image reference found in a workflow or action.
// At most one of Via, Local and Location is set.
type Image struct {
	// Ref is the `docker://…` value, `runs.image` Dockerfile path, or a job
	// container or service's plain image name.
	Ref string
	// Via is the remote locked action that runs the image (owner/repo@ref).
	// Its maintainer controls the reference.
	Via string
	// Local is the repository's own `$/…` action that runs the image.
	Local string
	// Location is where a workflow names the image, such as
	// `jobs.build.container` or `jobs.build.steps[2]`. A container or
	// service image is a plain name, without `docker://`.
	Location string
}

// Kind classifies i.Ref.
func (i Image) Kind() Kind {
	if i.Location != "" && !IsDockerUses(i.Ref) {
		return classifyName(i.Ref)
	}
	return Classify(i.Ref)
}

// RequiredBy is the dependency key of the remote action, owner/repo@ref,
// matching `required_by` on other findings; Via may add a sub-action path.
func (i Image) RequiredBy() string {
	name, ref, ok := strings.Cut(i.Via, "@")
	if !ok {
		return i.Via
	}
	if parts := strings.SplitN(name, "/", 3); len(parts) == 3 {
		name = parts[0] + "/" + parts[1]
	}
	return name + "@" + ref
}

// Mutable reports whether the image can change without a lockfile diff and
// is worth a warning. Digest pins are fixed; expressions and empty names
// can't be judged statically.
func (i Image) Mutable() bool {
	switch i.Kind() {
	case Tagged, Untagged, Built:
		return true
	}
	return false
}

// Name is the image as the registry knows it: `docker://alpine:3` and a job's
// `alpine:3` are the same image. A Dockerfile path is returned as is.
func (i Image) Name() string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(i.Ref), dockerPrefix))
}

// Detail explains why the image is not covered. It names the image by Name
// and omits Location, so one image used by several steps and jobs reads, and
// deduplicates, as one finding.
func (i Image) Detail() string {
	ref := i.Name()
	runner := i.Via
	if runner == "" {
		runner = i.Local
	}
	if i.Kind() == Built {
		what := "the image and its base images are built on the runner at run time"
		switch {
		case i.Via != "":
			return fmt.Sprintf("%s (built by %s) is not covered by the actions lockfile; the lock pins the action's commit, but %s", ref, i.Via, what)
		case i.Local != "":
			return fmt.Sprintf("%s (built by %s) is not covered by the actions lockfile; %s", ref, i.Local, what)
		}
		return fmt.Sprintf("%s is not covered by the actions lockfile; %s", ref, what)
	}
	why := "container image tags are mutable"
	if i.Kind() == Untagged {
		why = "an untagged image pulls the mutable latest tag"
	}
	switch {
	case i.Via != "":
		return fmt.Sprintf("%s (pulled by %s) is not covered by the actions lockfile; the lock pins the action's commit, not the image, and %s", ref, i.Via, why)
	case runner != "":
		return fmt.Sprintf("%s (pulled by %s) is not covered by the actions lockfile; %s", ref, runner, why)
	}
	return fmt.Sprintf("%s is not covered by the actions lockfile; %s", ref, why)
}

// Remediation suggests a digest pin from whoever controls the reference.
func (i Image) Remediation() string {
	switch {
	case i.Via != "" && i.Kind() == Built:
		return "ask the action's maintainer to publish a prebuilt image pinned by digest, or to pin the Dockerfile's base images by digest"
	case i.Via != "":
		return "ask the action's maintainer to pin the image by digest (image@sha256:…)"
	case i.Kind() == Built:
		return "pin the Dockerfile's base images by digest (FROM image@sha256:…), or run a prebuilt image pinned by digest"
	}
	return "pin the image by digest (image@sha256:…)"
}
