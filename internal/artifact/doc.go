// Package artifact is the content-addressed store for the evidence of every
// invocation — transcripts, gate output, candidate diffs — harvested before a
// sandbox is torn down and referenced thereafter by hash.
//
// Provenance is by construction: capturing evidence immutably, keyed by content,
// is what lets every merge trace back to its issue, soul, model, prompt, and proof.
package artifact
