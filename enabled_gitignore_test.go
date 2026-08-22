package main

import "testing"

func TestProjectIgnoreMatcherRequiresExcludedParentsToBeReincluded(t *testing.T) {
	matcher := projectIgnoreMatcher{}.append(
		"*\n!.local/\n!.local/share/\n!.local/share/keep.txt\n",
		nil,
	)
	for _, test := range []struct {
		name    string
		path    []string
		isDir   bool
		ignored bool
	}{
		{name: "allowlisted file", path: []string{".local", "share", "keep.txt"}},
		{name: "ignored sibling", path: []string{".local", "share", "containers"}, isDir: true, ignored: true},
		{name: "ignored sibling descendant", path: []string{".local", "share", "containers", "locked"}, isDir: true, ignored: true},
		{name: "unrelated basename", path: []string{"other", "keep.txt"}, ignored: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := matcher.match(test.path, test.isDir); got != test.ignored {
				t.Fatalf("match(%q, %v) = %v, want %v", test.path, test.isDir, got, test.ignored)
			}
		})
	}
}

func TestProjectIgnoreMatcherScopesNestedRules(t *testing.T) {
	matcher := projectIgnoreMatcher{}.
		append("*.secret\n", []string{"a"}).
		append("!file.secret\n", []string{"b"})
	if !matcher.match([]string{"a", "file.secret"}, false) {
		t.Fatal("nested exclusion was overridden by an unrelated sibling rule")
	}
	if matcher.match([]string{"b", "file.secret"}, false) {
		t.Fatal("nested exclusion leaked into a sibling directory")
	}
	if matcher.match([]string{"file.secret"}, false) {
		t.Fatal("nested exclusion leaked into the project root")
	}
}

func TestProjectIgnoreMatcherHandlesMalformedRulesConservatively(t *testing.T) {
	matcher := projectIgnoreMatcher{}.append("[\nfuture-ignore-format::value\nignored/\n", nil)
	if !matcher.match([]string{"ignored", "file.txt"}, false) {
		t.Fatal("valid rule after malformed rules was lost")
	}
	if matcher.match([]string{"unrelated.txt"}, false) {
		t.Fatal("malformed rule ignored an unrelated path")
	}
}

func TestProjectIgnoreMatcherPreservesEscapedTrailingSpace(t *testing.T) {
	matcher := projectIgnoreMatcher{}.append("secret\\  \n", nil)
	if !matcher.match([]string{"secret "}, false) {
		t.Fatal("escaped trailing-space rule did not match")
	}
	if matcher.match([]string{"secret"}, false) {
		t.Fatal("escaped trailing-space rule matched an unspaced name")
	}
}

func TestProjectIgnoreMatcherPreservesTrailingDoubleStarEndpoint(t *testing.T) {
	matcher := projectIgnoreMatcher{}.append("foo/**\n!foo/keep.ts\n", nil)
	if matcher.match([]string{"foo"}, true) {
		t.Fatal("trailing /** excluded its prefix directory")
	}
	if matcher.match([]string{"foo", "keep.ts"}, false) {
		t.Fatal("trailing /** prevented a visible descendant from being re-included")
	}
	if !matcher.match([]string{"foo", "drop.ts"}, false) {
		t.Fatal("trailing /** did not exclude a descendant")
	}

	nested := projectIgnoreMatcher{}.append("foo/**\n!foo/keep.ts\n", []string{"app"})
	if nested.match([]string{"app", "foo"}, true) {
		t.Fatal("nested trailing /** excluded its prefix directory")
	}
	if nested.match([]string{"app", "foo", "keep.ts"}, false) {
		t.Fatal("nested trailing /** prevented a visible descendant from being re-included")
	}
}

func TestProjectIgnoreMatcherPreservesDirectoryOnlySlashPattern(t *testing.T) {
	matcher := projectIgnoreMatcher{}.append("generated/package.json/\n", nil)
	if matcher.match([]string{"generated", "package.json"}, false) {
		t.Fatal("directory-only pattern excluded a regular file")
	}
	if !matcher.match([]string{"generated", "package.json"}, true) {
		t.Fatal("directory-only pattern did not exclude a directory")
	}

	nested := projectIgnoreMatcher{}.append("generated/package.json/\n", []string{"app"})
	if nested.match([]string{"app", "generated", "package.json"}, false) {
		t.Fatal("nested directory-only pattern excluded a regular file")
	}
	if !nested.match([]string{"app", "generated", "package.json"}, true) {
		t.Fatal("nested directory-only pattern did not exclude a directory")
	}
}
