// Package examples holds the annotated cluster.yaml and bundle.yaml
// templates, embedded so `searke gen` and the docs share one source.
package examples

import _ "embed"

//go:embed cluster.yaml
var Cluster string

//go:embed bundle.yaml
var Bundle string
