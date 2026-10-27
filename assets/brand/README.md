# Ordo · Folio

The selected identity: a serif wordmark with a gold diamond inside the initial O.
The same O is the standalone icon.

- `folio-light.svg` and `folio-dark.svg`: transparent README wordmarks.
- `folio-icon.svg`: editable icon and light-theme asset.
- `folio-icon-dark.svg`: generated dark-theme icon.
- `folio.typ`: editable wordmark layout and dark-theme colors.

The root README centers the wordmark in an accessible heading and selects the artwork for the visitor's theme.
The SVGs are self-contained, with outlined lettering; viewers need no fonts or Typst installation.
The web UI is unchanged.

## Regenerate

Run from the repository root.
Only generation needs Typst and the FreeSerif font (GNU FreeFont); these are not application dependencies.
The artwork was generated with Typst 0.15.1 and FreeFont 20120503.

```sh
export TYPST_FONT_PATHS="$(nix build --no-link --print-out-paths nixpkgs#freefont_ttf)"
for theme in light dark; do
  nix run nixpkgs#typst -- compile --ignore-system-fonts --input theme="$theme" \
    assets/brand/folio.typ "assets/brand/folio-$theme.svg"
done
nix run nixpkgs#typst -- compile --ignore-system-fonts --input mode=icon --input theme=dark \
  assets/brand/folio.typ assets/brand/folio-icon-dark.svg
```

Do not generate over `folio-icon.svg`: it is the source artwork, not a build output.
