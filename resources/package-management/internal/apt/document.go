package apt

import (
	"bytes"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
)

type AptFormat string

const (
	ListFormat   AptFormat = ".list"
	Deb822Format AptFormat = ".sources"
)

// AptDocument retains the original representation, including unmodeled fields
// and comments. Only changed physical entries are serialized.
type AptDocument struct {
	Path             string
	OriginalContents []byte
	Format           AptFormat
	Entries          []AptRepository
	original         []AptRepository
	blocks           []sourceBlock
	info             os.FileInfo
	store            Store
	create           bool
	parseErr         error
	replacement      []byte
}

type documentLine struct {
	start, end int
	text       string
}

func documentLines(data []byte) []documentLine {
	var lines []documentLine
	start := 0
	for _, line := range strings.SplitAfter(string(data), "\n") {
		if line == "" {
			continue
		}
		lines = append(lines, documentLine{start, start + len(line), strings.TrimSuffix(strings.TrimSuffix(line, "\n"), "\r")})
		start += len(line)
	}
	return lines
}

func parseDocument(path string, data []byte) (*AptDocument, error) {
	d := &AptDocument{Path: path, OriginalContents: slices.Clone(data), Format: AptFormat(filepath.Ext(path))}
	var err error
	switch d.Format {
	case ListFormat:
		err = d.parseList()
	case Deb822Format:
		err = d.parseDeb822()
	default:
		err = fmt.Errorf("unsupported APT document format %q", d.Format)
	}
	if err != nil {
		return nil, fmt.Errorf("parse APT source %q: %w; fix this source before reconciling repositories", path, err)
	}
	d.original = cloneRepositories(d.Entries)
	return d, nil
}

func cloneRepositories(entries []AptRepository) []AptRepository {
	out := slices.Clone(entries)
	for i := range out {
		out[i].Components = slices.Clone(out[i].Components)
		out[i].Architectures = slices.Clone(out[i].Architectures)
		out[i].Signing.Keyrings = slices.Clone(out[i].Signing.Keyrings)
		out[i].Signing.Fingerprints = slices.Clone(out[i].Signing.Fingerprints)
		out[i].Options = maps.Clone(out[i].Options)
	}
	return out
}

func (d *AptDocument) changed() bool {
	return d.create || d.replacement != nil || !reflect.DeepEqual(d.Entries, d.original)
}

func (d *AptDocument) contents() ([]byte, error) {
	if d.replacement != nil {
		return d.replacement, nil
	}
	if !d.changed() || d.create {
		return d.OriginalContents, nil
	}
	if len(d.Entries) != len(d.original) {
		return nil, fmt.Errorf("cannot append or reorder document entries; edit entries in place")
	}
	var out bytes.Buffer
	cursor := 0
	newline := "\n"
	if bytes.Contains(d.OriginalContents, []byte("\r\n")) {
		newline = "\r\n"
	}
	for _, block := range d.blocks {
		out.Write(d.OriginalContents[cursor:block.start])
		if reflect.DeepEqual(d.Entries[block.first:block.endEntry], d.original[block.first:block.endEntry]) {
			out.Write(d.OriginalContents[block.start:block.end])
		} else {
			written := false
			for index := block.first; index < block.endEntry; index++ {
				entry := d.Entries[index]
				if entry.removed {
					continue
				}
				if written && d.Format == Deb822Format {
					out.WriteString(newline)
				}
				var text string
				var err error
				if d.Format == ListFormat {
					text, err = serializeList(entry, strings.TrimRight(string(d.OriginalContents[block.start:block.end]), "\r\n"), newline)
				} else {
					text = d.renderDeb822(block, index, newline)
				}
				if err != nil {
					return nil, err
				}
				out.WriteString(text)
				written = true
			}
		}
		cursor = block.end
	}
	out.Write(d.OriginalContents[cursor:])
	return out.Bytes(), nil
}

func (d *AptDocument) renderDeb822(block sourceBlock, index int, newline string) string {
	before, after := repositoryFields(d.original[index]), repositoryFields(d.Entries[index])
	changes := map[string]string{}
	for name, value := range before {
		if value != after[name] {
			changes[name] = after[name]
		}
	}
	for name, value := range after {
		if value != before[name] {
			changes[name] = value
		}
	}
	if block.endEntry-block.first > 1 {
		for _, name := range []string{"types", "uris", "suites"} {
			changes[name] = after[name]
		}
	}
	var out strings.Builder
	cursor := block.start
	for _, field := range block.fields {
		out.Write(d.OriginalContents[cursor:field.start])
		if value, changed := changes[field.name]; changed {
			if value != "" {
				out.WriteString(deb822Field(fieldTitle(field.name), value, newline))
			}
			delete(changes, field.name)
		} else {
			out.Write(d.OriginalContents[field.start:field.end])
		}
		cursor = field.end
	}
	out.Write(d.OriginalContents[cursor:block.end])
	names := slices.Sorted(maps.Keys(changes))
	for _, name := range names {
		if changes[name] != "" {
			if out.Len() > 0 && !strings.HasSuffix(out.String(), "\n") {
				out.WriteString(newline)
			}
			out.WriteString(deb822Field(fieldTitle(name), changes[name], newline))
		}
	}
	// Split multi-value stanzas must remain separate even without a final newline.
	if block.endEntry-block.first > 1 && !strings.HasSuffix(out.String(), "\n") {
		out.WriteString(newline)
	}
	return out.String()
}

func (d *AptDocument) preflight() error {
	if !d.changed() {
		return nil
	}
	content, err := d.contents()
	if err != nil {
		return err
	}
	if _, err := parseDocument(d.Path, content); err != nil {
		return err
	}
	return d.store.checkWrite(d.Path, d.OriginalContents, d.info, d.create)
}

func (d *AptDocument) WriteToDisk() error {
	if !d.changed() {
		return nil
	}
	content, err := d.contents()
	if err != nil {
		return err
	}
	if !d.create && bytes.Equal(content, d.OriginalContents) {
		d.original = cloneRepositories(d.Entries)
		return nil
	}
	if err := d.preflight(); err != nil {
		return err
	}
	mode := os.FileMode(0644)
	if d.info != nil {
		mode = d.info.Mode().Perm()
	}
	if err := atomicWrite(d.Path, content, mode); err != nil {
		return fmt.Errorf("write APT document %q: %w", d.Path, err)
	}
	reloaded, err := d.store.readDocument(d.Path)
	if err != nil {
		return err
	}
	*d = *reloaded
	return nil
}
