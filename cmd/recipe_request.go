package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"zenget/internal/github"
	"zenget/internal/provider"
	"zenget/internal/reciperequest"
	"zenget/internal/registry"
	"zenget/internal/version"
)

var newRecipeRequestClient = reciperequest.New

func githubComUpstream(client provider.Provider) bool {
	c, ok := client.(*github.Client)
	return ok && c.ProviderName() == "github" && strings.TrimRight(c.BaseURL, "/") == "https://api.github.com"
}

func offerRecipeRequest(ctx context.Context, client provider.Provider, repository string, options installOptions, missing []missingRegistryRecipe, tag string, installErr error, out io.Writer) {
	if !options.allowRecipeRequest || !options.interactive || options.offline || os.Getenv("CI") != "" || ctx.Err() != nil || errors.Is(installErr, context.Canceled) || errors.Is(installErr, context.DeadlineExceeded) || !githubComUpstream(client) || options.prompter == nil || options.prompter.Aborted() {
		return
	}
	selected := missing[0]
	if len(missing) > 1 {
		choices := make([]string, len(missing))
		for i, candidate := range missing {
			choices[i] = candidate.name + " (" + candidate.source.RepositoryName() + ")"
		}
		choice, err := options.prompter.Choose("Choose a registry for the missing recipe request", choices)
		if err != nil || options.prompter.Aborted() {
			return
		}
		for i, value := range choices {
			if value == choice {
				selected = missing[i]
				break
			}
		}
	}
	title, body := recipeRequestPreview(repository, options, selected, tag, installErr == nil)
	target := selected.source.RepositoryName()
	if _, err := fmt.Fprintf(out, "\nMissing registry recipe. Proposed GitHub issue in %s\nTitle: %s\n\n%s\n", target, title, body); err != nil {
		return
	}
	consent, err := options.prompter.Confirm("Submit this recipe request to GitHub?")
	if err != nil || !consent || ctx.Err() != nil {
		return
	}
	manual := "https://github.com/" + target + "/issues/new"
	fail := func(reason string) {
		_, _ = fmt.Fprintf(out, "Recipe request creation is not confirmed: %s. No automatic retry. Check existing issues before submitting manually: %s\n", reason, manual)
	}
	requestClient := newRecipeRequestClient()
	if requestClient.Token == "" {
		fail("set ZENGET_GITHUB_TOKEN or GITHUB_TOKEN (Issues write permission required)")
		return
	}
	// Authorization is rechecked immediately before looking at the current index.
	if checkRegistryPolicy(selected.source, registry.IndexFileName) != nil {
		fail("registry source policy no longer allows this root")
		return
	}
	exists, err := requestClient.RecipeExists(ctx, selected.source, repository)
	if err != nil {
		fail(err.Error())
		return
	}
	if exists {
		_, _ = fmt.Fprintf(out, "The current registry already contains a recipe for %s. Update registry %s's commit pin with registry remove/add, then run zenget registry sync %s; the local snapshot and install source were not changed.\n", repository, selected.name, selected.name)
		return
	}
	duplicate, err := requestClient.FindOpen(ctx, target, repository)
	if err != nil {
		fail(err.Error())
		return
	}
	if duplicate != "" {
		_, _ = fmt.Fprintf(out, "An open recipe request already exists: %s\n", duplicate)
		return
	}
	created, err := requestClient.Create(ctx, target, title, body)
	if err != nil {
		fail(err.Error())
		return
	}
	_, _ = fmt.Fprintf(out, "Created recipe request: %s\n", created)
}

func recipeRequestPreview(repository string, options installOptions, selected missingRegistryRecipe, tag string, succeeded bool) (string, string) {
	if tag == "" {
		tag = "unknown"
	}
	root := selected.source.Root
	if root == "" {
		root = "(repository root)"
	}
	outcome := "failed"
	if succeeded {
		outcome = "succeeded"
	}
	// Release tags come from an upstream API; quote them so multiline content
	// cannot inject metadata or a second project marker into the request.
	body := fmt.Sprintf("%s\n\nPlease add a registry recipe for this upstream project.\n\nUpstream: https://github.com/%s\nTarget platform: %s/%s\nzenget version: %s\nKnown release tag: %q\nRegistry name: %s\nRegistry root: %s\nSnapshot commit: %s\nInstallation: %s\n", reciperequest.Marker(repository), repository, options.goos, options.goarch, version.Version, tag, selected.name, root, selected.source.Commit, outcome)
	return "Recipe request: " + repository, body
}
