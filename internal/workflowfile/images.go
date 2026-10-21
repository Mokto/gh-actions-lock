package workflowfile

import (
	"strings"

	"github.com/github/gh-actions-lock/internal/uses"
	"gopkg.in/yaml.v3"
)

// ContainerImages returns the unpinned images the workflow's jobs run
// directly: `docker://` steps, the job container and service containers.
func (f *File) ContainerImages() []uses.Image {
	jobs := mapValue(resolveAlias(documentRoot(&f.root)), "jobs")
	if jobs == nil || jobs.Kind != yaml.MappingNode {
		return nil
	}
	var out []uses.Image
	add := func(raw, where string) {
		if img, ok := uses.ClassifyImage(raw); ok {
			img.Where = where
			out = append(out, img)
		}
	}
	for i := 0; i < len(jobs.Content)-1; i += 2 {
		job := resolveAlias(jobs.Content[i+1])
		if job == nil || job.Kind != yaml.MappingNode {
			continue
		}
		where := "job " + jobs.Content[i].Value
		walkStepUses(job, jobSteps, func(value string, _ bool) {
			if strings.HasPrefix(strings.TrimSpace(value), "docker://") {
				add(value, where)
			}
		})
		add(containerImage(mapValue(job, "container")), where+" container")
		services := mapValue(job, "services")
		if services == nil || services.Kind != yaml.MappingNode {
			continue
		}
		for j := 0; j < len(services.Content)-1; j += 2 {
			add(containerImage(resolveAlias(services.Content[j+1])), where+" service "+services.Content[j].Value)
		}
	}
	return out
}

// containerImage reads a `container:` or service, which is an image name or
// a mapping with `image:`.
func containerImage(node *yaml.Node) string {
	if node != nil && node.Kind == yaml.MappingNode {
		return scalarValue(mapValue(node, "image"))
	}
	return scalarValue(node)
}
