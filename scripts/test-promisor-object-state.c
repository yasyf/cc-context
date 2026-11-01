#define USE_THE_REPOSITORY_VARIABLE
#include "git-compat-util.h"
#include "test-tool.h"
#include "commit.h"
#include "commit-graph.h"
#include "hex.h"
#include "object.h"
#include "odb.h"
#include "packfile.h"
#include "promisor-remote.h"
#include "repository.h"
#include "setup.h"
#include "tree.h"

int cmd__promisor_object_state(int argc, const char **argv)
{
	struct repository *r = the_repository;
	struct object_id source, target, marker;
	struct object *object;
	struct commit *graph_commit;
	int parsed_before, promised, raw, reset;
	uint32_t graph_position;

	if (argc != 5)
		usage("test-tool promisor-object-state <fresh|parsed|raw|reset> <source-oid> <target-oid> <graph-commit>");
	if (strcmp(argv[1], "fresh") && strcmp(argv[1], "parsed") &&
	    strcmp(argv[1], "raw") && strcmp(argv[1], "reset"))
		die("unexpected diagnostic mode");
	raw = !strcmp(argv[1], "raw") || !strcmp(argv[1], "reset");
	reset = !strcmp(argv[1], "reset");
	setup_git_directory(r);
	if (get_oid_hex_algop(argv[2], &source, r->hash_algo) ||
	    get_oid_hex_algop(argv[3], &target, r->hash_algo) ||
	    get_oid_hex_algop(argv[4], &marker, r->hash_algo))
		die("invalid diagnostic object ID");
	if (!repo_has_promisor_remote(r))
		die("diagnostic fixture has no promisor remote");
	if (raw) {
		struct object_info oi = OBJECT_INFO_INIT;
		enum object_type type;
		size_t size;
		void *buffer;
		int eaten;

		oi.typep = &type;
		oi.sizep = &size;
		oi.contentp = &buffer;
		if (odb_read_object_info_extended(r->objects, &source, &oi,
						  OBJECT_INFO_SKIP_FETCH_OBJECT))
			die("cannot read physical diagnostic source");
		object = parse_object_buffer(r, &source, type, size, buffer, &eaten);
		if (!eaten)
			free(buffer);
		if (!object)
			die("cannot parse physical diagnostic source");
		if (reset) {
			switch (object->type) {
			case OBJ_COMMIT:
				unparse_commit(r, &source);
				break;
			case OBJ_TREE:
				free_tree_buffer((struct tree *)object);
				break;
			default:
				die("diagnostic reset supports only commits and trees");
			}
		}
	} else if (!strcmp(argv[1], "parsed") &&
		   !parse_object_with_flags(r, &source, PARSE_OBJECT_SKIP_HASH_CHECK)) {
		die("cannot parse contextual diagnostic source");
	}
	object = lookup_object(r, &source);
	parsed_before = object && object->parsed;
	promised = is_promisor_object(r, &target);
	object = lookup_object(r, &source);
	if (!object)
		die("promisor traversal did not visit diagnostic source");
	graph_commit = lookup_commit(r, &marker);
	if (!graph_commit || !graph_commit->object.parsed)
		die("promisor traversal did not parse graph marker");
	graph_position = commit_graph_position(graph_commit);
	printf("{\"parsed_before\":%d,\"parsed_after\":%d,\"source_type\":%u,\"promised\":%d,\"graph_position\":%"PRIu32"}\n",
	       parsed_before, object->parsed, object->type, promised, graph_position);
	return 0;
}
