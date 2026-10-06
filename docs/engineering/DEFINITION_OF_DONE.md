# Definition of done

A change is done when every applicable item below is satisfied:

- [ ] The change matches an approved specification and remains within scope.
- [ ] Architecture and module boundaries are preserved, or an ADR was accepted
      before the architectural change was implemented.
- [ ] Ownership of persistent data remains explicit and PostgreSQL remains the
      source of truth.
- [ ] Tests cover new behavior and important failure paths.
- [ ] `go test ./...` passes from the repository root.
- [ ] Mandatory database tests actually execute against the release schema.
      Missing/unreachable PostgreSQL, setup failure, or an unexpected skip
      fails the gate. Test counts and skipped-test reasons are recorded;
      a green exit code or a later migration smoke test is not sufficient.
- [ ] Applicable [release gates](RELEASE_GATES.md) have evidence tied to
      backend/frontend commits, schema, API contract and configuration.
      Documentation-only work explicitly distinguishes corrected requirements
      from runtime behavior that remains unverified.
- [ ] Documentation and relevant specifications reflect the resulting behavior.
- [ ] Normative pages and derived editions follow [documentation governance](DOCS_GOVERNANCE.md);
      accepted ADR history and historical test reports are not presented as
      fresh verification. Required external artifacts are available or remain
      explicitly blocked in the [dependency register](EXTERNAL_ARTIFACTS.md).
- [ ] Errors and logs do not disclose secrets or sensitive data.
- [ ] No unnecessary production dependency, generated artifact, or unrelated
      refactor is included.
- [ ] The final change summary identifies verification performed and any known
      limitations.
