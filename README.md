# News

A minimalistic static-site RSS/Atom feed aggregator in a few hundred lines of
Go code.

This is my daily driver for reading news. It supports mobile, vim-style
keyboard navigation and dark mode.

## Usage

Edit `./feeds.json` to setup your feeds, then run `news` to generate HTML
output in `./html/`.

For command-line options (including changing the `feeds.json` path or the
output directory), run `news --help`.
