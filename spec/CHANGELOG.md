# Spec changelog

## Additions since v0.4 — SDK-5989

- One-shot interactions use the interaction ID as the required conversation ID with explicit server opt-in; chat defaults and existing 0.3.0 captures remain compatible.
- Client events cover web and mobile SDKs (JavaScript, iOS, Android, React Native, Flutter).
- Add actionable suggestion exposure and reversion with item counts; scoped feedback, preset reasons, dismissal and request triggers; exports with snake_case destination labels.
- Add conversation pin/share, agent/mode selection, assistant entry/close methods, prompt input/context metadata, client limits and declared client tool outcomes.
- Extend conformance and migration fixtures. Existing schema filenames, schema IDs and SPEC_VERSION remain unchanged; a later stacked layer performs the version rename.
