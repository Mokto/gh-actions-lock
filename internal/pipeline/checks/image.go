package checks

import (
	"fmt"
	"slices"
	"strings"

	"github.com/github/gh-actions-lock/internal/uses"
)

// AddContainerImage records img on wr's container-image finding for its
// image, adding the finding on first use, so each image is reported once per
// workflow with every place it's used.
func AddContainerImage(wr *WorkflowReport, img uses.Image) {
	for i := range wr.Findings {
		f := &wr.Findings[i]
		if f.Category != ContainerImage || ImageHeadline(f.ImageUses[0]) != ImageHeadline(img) {
			continue
		}
		if !slices.Contains(f.ImageUses, img) {
			f.ImageUses = append(f.ImageUses, img)
			describeImage(f)
		}
		return
	}
	f := Finding{
		WorkflowPath: wr.Path,
		Category:     ContainerImage,
		Severity:     SeverityWarning,
		Confidence:   ConfidenceHigh,
		ImageUses:    []uses.Image{img},
	}
	describeImage(&f)
	wr.Findings = append(wr.Findings, f)
}

// ImageHeadline says what is unpinned about img. Uses with the same
// headline share one finding.
func ImageHeadline(img uses.Image) string {
	switch img.Kind {
	case uses.ImageExpression:
		return fmt.Sprintf("container image `%s` is chosen at runtime, so the lockfile can't check it", img.Name)
	case uses.ImageBuild:
		return fmt.Sprintf("%s builds its container from %s; the lockfile doesn't cover its base image", img.Action, img.Name)
	}
	return fmt.Sprintf("container image %s is not pinned by digest", img.Name)
}

func describeImage(f *Finding) {
	img := f.ImageUses[0]
	var where, actions []string
	repoOwned := false
	for _, u := range f.ImageUses {
		where = append(where, u.Where)
		switch {
		case u.Action == "":
			repoOwned = true
		case !slices.Contains(actions, u.Action):
			actions = append(actions, u.Action)
		}
	}
	f.Detail = fmt.Sprintf("%s; used by %s", ImageHeadline(img), strings.Join(where, ", "))
	f.ParentNWO = ""
	if !repoOwned && len(actions) == 1 {
		f.ParentNWO = actions[0]
	}
	switch {
	case img.Kind == uses.ImageExpression:
		f.Remediation = "use a literal image pinned by digest where possible, or pin each value of the matrix or input by digest"
	case img.Kind == uses.ImageBuild:
		f.Remediation = fmt.Sprintf("only the maintainer of %s can pin its base image by digest", img.Action)
	case repoOwned:
		f.Remediation = fmt.Sprintf("the lockfile pins action commits, not container images; pin it as %s@sha256:…", img.Name)
	default:
		f.Remediation = fmt.Sprintf("the lockfile pins action commits, not container images; only the maintainer of %s can pin it by digest", strings.Join(actions, ", "))
	}
}
