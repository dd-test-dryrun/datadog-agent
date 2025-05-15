package tagprovider

import (
	taggertypes "github.com/DataDog/datadog-agent/comp/core/tagger/types"
)

type TagProvider[T any] interface {
	GetTags(T, taggertypes.TagCardinality) ([]string, error)
}

type TagProviderFunc[T any] func(T, taggertypes.TagCardinality) ([]string, error)

func (f TagProviderFunc[T]) GetTags(t T, c taggertypes.TagCardinality) ([]string, error) {
	return f(t, c)
}
