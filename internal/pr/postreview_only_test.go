package pr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// postsReview reports whether the string literal s is one a review post is
// built from: the `review` subcommand of `gh pr review`, a REST path to a
// pull request's reviews (whole, or its "reviews" segment on its own), or the
// GraphQL mutation that adds a review.
func postsReview(s string) bool {
	return s == "review" || s == "reviews" || strings.Contains(s, "/reviews") ||
		strings.Contains(s, "addPullRequestReview")
}

// PostReview is the only function in this package that builds a review post,
// and in it the token scan runs before the post (forgectl#754, #681). A new
// verb that posts a review some other way, by `gh pr review` or `gh api
// …/reviews` or the GraphQL mutation, would skip the scan; this fails the
// moment such a literal appears outside PostReview, so the new caller has to
// route through PostReview instead.
//
// It is scoped to this package, internal/pr; a poster built in another
// package is TestPostReview_NoReviewPosterOutsideThePackage's. Within it, it is a syntactic check over the non-test
// files: every string literal is read, wherever it sits (a call's argv, a
// slice literal built up for one later, a constant), so an argv assembled
// away from the Run call is still seen. It also requires that PostReview
// itself holds at least one such literal, so a scan that finds nothing
// anywhere cannot pass.
//
// Mutations that turn it red: add `func postElsewhere(ctx context.Context, r
// Runner) { _, _ = r.Run(ctx, "gh", "pr", "review") }` to a non-test file
// (a literal outside PostReview); a `const reviewsPath =
// "repos/%s/pulls/%d/reviews"` anywhere outside it; a path joined from a bare
// "reviews" segment; delete the scanReviewForTokens call from PostReview, or
// move it below the gh argv (the scan no longer precedes the post).
func TestPostReview_IsTheOnlyReviewPoster(t *testing.T) {
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var insidePost []token.Pos
	var scanPos token.Pos
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// Every review-post literal, and the function (if any) that holds it.
		var enclosing *ast.FuncDecl
		ast.Inspect(f, func(n ast.Node) bool {
			if fd, ok := n.(*ast.FuncDecl); ok {
				enclosing = fd
			}
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			s, err := strconv.Unquote(lit.Value)
			if err != nil || !postsReview(s) {
				return true
			}
			inPost := enclosing != nil && enclosing.Name.Name == "PostReview" && enclosing.Recv != nil &&
				lit.Pos() >= enclosing.Pos() && lit.End() <= enclosing.End()
			if !inPost {
				t.Errorf("%s: review-post literal %q outside PostReview; post through PostReview so its token scan runs",
					fset.Position(lit.Pos()), s)
				return true
			}
			insidePost = append(insidePost, lit.Pos())
			return true
		})
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != "PostReview" || fd.Recv == nil || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "scanReviewForTokens" && scanPos == token.NoPos {
					scanPos = call.Pos()
				}
				return true
			})
		}
	}
	if len(insidePost) == 0 {
		t.Fatal("no review-post literal found in PostReview; the check cannot see the post it guards")
	}
	if scanPos == token.NoPos {
		t.Fatal("PostReview does not call scanReviewForTokens")
	}
	for _, p := range insidePost {
		if p < scanPos {
			t.Errorf("%s: PostReview builds a review post before its scanReviewForTokens call", fset.Position(p))
		}
	}
}

// reviewLiteralAllowlist is every file outside internal/pr allowed to hold a
// literal postsReview matches, with exactly how many it holds. None of them
// posts a review: the `forgectl init` scaffold row for the review workflow
// (its name and config key), the review workflow's own Name and ConfigKey, and
// the review workflow's launch export. The count is exact, so a new literal
// in an allowlisted file fails as surely as one anywhere else, and so does an
// entry whose literals are gone.
var reviewLiteralAllowlist = map[string]int{
	"internal/cli/init_cmd.go": 2,
	"internal/cli/review.go":   2,
	"internal/launch/steps.go": 1,
}

// TestPostReview_NoReviewPosterOutsideThePackage widens
// TestPostReview_IsTheOnlyReviewPoster to the module (forgectl#776): a verb
// in another package that builds `gh pr review`, `gh api …/reviews`, or the
// GraphQL mutation would post without PostReview's token scan, and the
// in-package check cannot see it. It reads every string literal in every
// non-test .go file outside internal/pr (which the in-package test owns) and
// fails on any postsReview match that reviewLiteralAllowlist does not account
// for.
//
// Mutations that turn it red: add `var reviewsPath =
// "repos/%s/pulls/%d/reviews"` to a non-test file in internal/cli; add a third
// "review" literal to internal/cli/review.go; or remove the scaffold row from
// init_cmd.go (its allowlist entry goes stale).
func TestPostReview_NoReviewPosterOutsideThePackage(t *testing.T) {
	root, err := os.OpenRoot(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if _, err := root.Stat("go.mod"); err != nil {
		t.Fatalf("the module root is not two levels up: %v", err)
	}
	fsys := root.FS()
	fset := token.NewFileSet()
	got := map[string]int{}
	err = fs.WalkDir(fsys, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch {
			case path == "internal/pr", path != "." && strings.HasPrefix(d.Name(), "."),
				d.Name() == "testdata", d.Name() == "vendor", d.Name() == "node_modules":
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(fset, path, src, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			if s, err := strconv.Unquote(lit.Value); err == nil && postsReview(s) {
				got[path]++
				if _, allowed := reviewLiteralAllowlist[path]; !allowed {
					t.Errorf("%s: review-post literal %q outside internal/pr; post through pr.PostReview so its token scan runs",
						fset.Position(lit.Pos()), s)
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module: %v", err)
	}
	for path, want := range reviewLiteralAllowlist {
		if got[path] != want {
			t.Errorf("%s holds %d review-post literals, allowlisted for %d; "+
				"a new one must post through pr.PostReview, and a removed one must leave the allowlist",
				path, got[path], want)
		}
	}
}
