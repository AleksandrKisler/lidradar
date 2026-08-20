# Definition of done

A change is done when every applicable item below is satisfied:

- [ ] The change matches an approved specification and remains within scope.
- [ ] Architecture and module boundaries are preserved, or an ADR was accepted
      before the architectural change was implemented.
- [ ] Ownership of persistent data remains explicit and PostgreSQL remains the
      source of truth.
- [ ] Tests cover new behavior and important failure paths.
- [ ] `go test ./...` passes from the repository root.
- [ ] Documentation and relevant specifications reflect the resulting behavior.
- [ ] Errors and logs do not disclose secrets or sensitive data.
- [ ] No unnecessary production dependency, generated artifact, or unrelated
      refactor is included.
- [ ] The final change summary identifies verification performed and any known
      limitations.
