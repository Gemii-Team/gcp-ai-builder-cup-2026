//go:build !linux

package pdftext

// confine would limit what the child may use. Only the Linux build has rlimits worth
// relying on here: the server runs on Linux, and elsewhere the parent's deadline and
// the child's own budget are all there is.
func confine() error { return nil }
