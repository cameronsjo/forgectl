package pr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// postsReview reports whether the string literal s is one a review post is
// built from: the `review` subcommand of `gh pr review`, a REST path to a
// pull request's reviews, or the GraphQL mutation that adds a review.
func postsReview(s string) bool {
	return s == "review" || strings.Contains(s, "/reviews") || strings.Contains(s, "addPullRequestReview")
}

// PostReview is the only function in this package that builds a review post,
// and in it the token scan runs before the post (forgectl#754, #681). A new
// verb that posts a review some other way, by `gh pr review` or `gh api
// …/reviews` or the GraphQL mutation, would skip the scan; this fails the
// moment such a literal appears outside PostReview, so the new caller has to
// route through PostReview instead.
//
// It is a syntactic check over the package's non-test files: every string
// literal is read, wherever it sits (a call's argv, a slice literal built up
// for one later, a constant), so an argv assembled away from the Run call is
// still seen. It also requires that PostReview itself holds at least one such
// literal, so a scan that finds nothing anywhere cannot pass.
//
// Mutations that turn it red: add `func postElsewhere(ctx context.Context, r
// Runner) { _, _ = r.Run(ctx, "gh", "pr", "review") }` to a non-test file
// (a literal outside PostReview); a `const reviewsPath =
// "repos/%s/pulls/%d/reviews"` anywhere outside it; delete the
// scanReviewForTokens call from PostReview, or move it below the gh argv
// (the scan no longer precedes the post).
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
