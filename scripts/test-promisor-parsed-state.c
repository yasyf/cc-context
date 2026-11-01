#define USE_THE_REPOSITORY_VARIABLE
#include "git-compat-util.h"
#include "test-tool.h"
#include "commit.h"
#include "hex.h"
#include "packfile.h"
#include "promisor-remote.h"
#include "repository.h"
#include "setup.h"

int cmd__promisor_parsed_state(int argc, const char **argv)
{
	struct repository *r = the_repository;
	struct object_id source, target;
	struct commit *commit;
	struct commit_graft *initial, *replacement;
	int preparsed, explicit_unparse, parsed_before, promised;
	unsigned int parents_before;

	if (argc != 4)
		usage("test-tool promisor-parsed-state <fresh|parsed|unparsed> <source-oid> <target-oid>");
	preparsed = !strcmp(argv[1], "parsed") || !strcmp(argv[1], "unparsed");
	explicit_unparse = !strcmp(argv[1], "unparsed");
	if (!preparsed && strcmp(argv[1], "fresh"))
		die("unexpected diagnostic mode");
	setup_git_directory();
	if (get_oid_hex_algop(argv[2], &source, r->hash_algo) ||
	    get_oid_hex_algop(argv[3], &target, r->hash_algo))
		die("invalid diagnostic object ID");
	if (!repo_has_promisor_remote(r))
		die("diagnostic fixture has no promisor remote");
	prepare_commit_graft(r);
	initial = xcalloc(1, sizeof(*initial));
	oidcpy(&initial->oid, &source);
	if (register_commit_graft(r, initial, 0))
		die("diagnostic source already has a graft");
	commit = lookup_commit(r, &source);
	if (!commit || (preparsed && repo_parse_commit(r, commit)))
		die("cannot parse diagnostic source commit");
	replacement = xcalloc(1, st_add(sizeof(*replacement), sizeof(struct object_id)));
	oidcpy(&replacement->oid, &source);
	replacement->nr_parent = 1;
	oidcpy(&replacement->parent[0], &target);
	if (!register_commit_graft(r, replacement, 0))
		die("diagnostic graft was not replaced");
	if (explicit_unparse)
		unparse_commit(r, &source);
	parsed_before = commit->object.parsed;
	parents_before = commit_list_count(commit->parents);
	promised = is_promisor_object(r, &target);
	if (!commit->object.parsed)
		die("promisor traversal did not parse the fixture source");
	printf("{\"parsed_before\":%d,\"parents_before\":%u,\"promised\":%d,\"parents_after\":%u}\n",
	       parsed_before, parents_before, promised, commit_list_count(commit->parents));
	return 0;
}
