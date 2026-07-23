"""Syntax tree shaped input for greedy sibling packing."""


class GreedySiblingPacker:
    def collect_python_nodes(self, source_text: str) -> list[str]:
        def keep_significant_lines(lines: list[str]) -> list[str]:
            kept = []
            for line in lines:
                stripped = line.strip()
                if stripped and not stripped.startswith("#"):
                    kept.append(stripped)
            return kept

        raw_lines = source_text.splitlines()
        significant_lines = keep_significant_lines(raw_lines)
        return [f"python:{line}" for line in significant_lines]

    def collect_go_nodes(self, source_text: str) -> list[str]:
        def keep_declarations(lines: list[str]) -> list[str]:
            declarations = []
            for line in lines:
                stripped = line.strip()
                if stripped.startswith(("func ", "type ", "var ", "const ")):
                    declarations.append(stripped)
            return declarations

        source_lines = source_text.splitlines()
        declaration_lines = keep_declarations(source_lines)
        return [f"go:{line}" for line in declaration_lines]

    def pack_adjacent_siblings(self, nodes: list[str], target_chars: int = 750) -> list[str]:
        groups = []
        current = []
        current_size = 0
        for node in nodes:
            node_size = len(node)
            if current and current_size + node_size > target_chars:
                groups.append("\n".join(current))
                current = []
                current_size = 0
            current.append(node)
            current_size += node_size
        if current:
            groups.append("\n".join(current))
        return groups

    def merge_small_adjacent_groups(self, groups: list[str], minimum_chars: int = 50) -> list[str]:
        merged = []
        pending = ""
        for group in groups:
            candidate = f"{pending}\n{group}".strip() if pending else group
            if len(candidate) < minimum_chars:
                pending = candidate
                continue
            merged.append(candidate)
            pending = ""
        if pending:
            if merged:
                merged[-1] = f"{merged[-1]}\n{pending}"
            else:
                merged.append(pending)
        return merged


def build_chunk_plan(source_by_language: dict[str, str]) -> dict[str, list[str]]:
    packer = GreedySiblingPacker()

    def select_nodes(language: str, source_text: str) -> list[str]:
        if language == "python":
            return packer.collect_python_nodes(source_text)
        if language == "go":
            return packer.collect_go_nodes(source_text)
        return [line.strip() for line in source_text.splitlines() if line.strip()]

    plans = {}
    for language, source_text in source_by_language.items():
        nodes = select_nodes(language, source_text)
        packed = packer.pack_adjacent_siblings(nodes)
        plans[language] = packer.merge_small_adjacent_groups(packed)
    return plans
