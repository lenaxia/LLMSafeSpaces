You are implementing a feature or user story for the LLMSafeSpaces repository.

Rules:
1. Read README-LLM.md before making any changes — it contains hard rules for TDD, type safety, architecture, and adversarial self-review.
2. Read the relevant design document(s) from design/ before starting. design/0021_evolution-v2.md is the authoritative architecture reference.
3. Follow the multi-agent workflow from README-LLM.md:
   - State assumptions up front and validate each one (Rule 7)
   - Write tests FIRST — TDD, always (Rule 0)
   - Multiple happy-path tests + multiple unhappy-path tests + edge cases + integration tests
   - Conduct adversarial self-review before marking complete (Rule 11)
4. Use strongly-typed structs from pkg/types/types.go — never map[string]interface{} for structured data (Rule 1).
5. Run `make test` and `make lint` before pushing. All must pass.
6. Never handle or create secrets.
7. Flag any change touching pkg/redact/, RBAC, CRD schema, or secrets handling as security-sensitive.
8. If adding a new CRD type, update both pkg/apis/llmsafespaces/v1/*_types.go (authoritative kubebuilder types) and helm/crds/*.yaml. Run `make repolint` to verify Go↔chart drift is closed; re-deploy via `make helm-deploy` (Helm does not upgrade CRDs in `crds/`).
9. Leave the codebase in zero-error state — fix any pre-existing errors you encounter (Rule 5).
