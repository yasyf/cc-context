// bridge.c exports a minimal C ABI over the tree-sitter runtime for one grammar,
// selected at compile time via -DTS_LANGUAGE=tree_sitter_<lang>. wazero loads the
// compiled module and drives it: ts_alloc a source buffer, ts_parse it into a
// flat pre-order node array, then ts_free the result.
//
// Output buffer layout (little-endian uint32):
//   [0]           node count N
//   [1 + 3*i .. ] for node i in pre-order DFS: start_byte, end_byte, child_count
// Pre-order + child_count lets the caller rebuild the tree without kinds.

#include <stdint.h>
#include <stdlib.h>
#include <string.h>

#include "tree_sitter/api.h"

extern const TSLanguage *TS_LANGUAGE(void);

__attribute__((export_name("ts_alloc"))) void *ts_alloc(uint32_t len) {
	return malloc(len);
}

__attribute__((export_name("ts_free"))) void ts_free(void *ptr) {
	free(ptr);
}

// vec is a growable uint32 buffer for the emitted node array.
typedef struct {
	uint32_t *data;
	uint32_t len;
	uint32_t cap;
} vec;

static void vec_push(vec *v, uint32_t x) {
	if (v->len == v->cap) {
		v->cap = v->cap ? v->cap * 2 : 1024;
		v->data = realloc(v->data, v->cap * sizeof(uint32_t));
	}
	v->data[v->len++] = x;
}

// stack is an explicit DFS work stack of nodes, avoiding C recursion on deep trees.
typedef struct {
	TSNode *data;
	uint32_t len;
	uint32_t cap;
} stack;

static void stack_push(stack *s, TSNode n) {
	if (s->len == s->cap) {
		s->cap = s->cap ? s->cap * 2 : 256;
		s->data = realloc(s->data, s->cap * sizeof(TSNode));
	}
	s->data[s->len++] = n;
}

__attribute__((export_name("ts_parse"))) uint64_t ts_parse(const char *src, uint32_t len) {
	TSParser *parser = ts_parser_new();
	ts_parser_set_language(parser, TS_LANGUAGE());
	TSTree *tree = ts_parser_parse_string(parser, NULL, src, len);
	TSNode root = ts_tree_root_node(tree);

	vec out = {0};
	vec_push(&out, 0); // node-count placeholder at index 0
	uint32_t count = 0;

	stack st = {0};
	stack_push(&st, root);
	while (st.len > 0) {
		TSNode n = st.data[--st.len];
		uint32_t nchildren = ts_node_child_count(n);
		vec_push(&out, ts_node_start_byte(n));
		vec_push(&out, ts_node_end_byte(n));
		vec_push(&out, nchildren);
		count++;
		// Push children in reverse so they pop in source order (pre-order DFS).
		for (uint32_t i = nchildren; i > 0; i--) {
			stack_push(&st, ts_node_child(n, i - 1));
		}
	}
	out.data[0] = count;
	free(st.data);

	ts_tree_delete(tree);
	ts_parser_delete(parser);

	return ((uint64_t)(uintptr_t)out.data << 32) | (uint64_t)(out.len * sizeof(uint32_t));
}
