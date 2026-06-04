# CJK Font Placement for Year-End Adjustment PDF Rendering

## Purpose

This directory holds the TrueType font file(s) used by `internal/yearend/pdf.go`
to render Japanese (CJK) text in 源泉徴収票 and 法定調書合計表 PDFs.

The font file is loaded at runtime via `embed.FS`. When it is absent the PDF
renderer falls back to Helvetica with romanised labels so that builds and tests
always succeed without the binary asset.

## Required file

| Filename     | Font                | License                |
|--------------|---------------------|------------------------|
| `ipaexg.ttf` | IPAex Gothic (IPA)  | IPA Font License v1.0  |

## How to obtain and place the font

1. Download the latest IPAex font archive from the official IPA page:
   <https://moji.or.jp/ipafont/ipaex00401/>

2. Extract `ipaexg.ttf` from the archive.

3. Copy it to this directory:
   ```
   cp /path/to/ipaexg.ttf backend/internal/yearend/fonts/ipaexg.ttf
   ```

4. **Do NOT commit the font binary to the repository.** Add it to each
   deployment environment (Docker image build step, CI artifact cache, etc.)
   as described in the deployment runbook.

## License compliance

When distributing PDFs generated with this font you must comply with the
[IPA Font License v1.0](https://moji.or.jp/ipafont/license/).  Key obligations:

- Include the IPA Font License text alongside any redistribution of the font file.
- Do not modify the font file and redistribute it under the IPA font name.

If a different CJK font is used instead, ensure its license permits the
intended distribution (commercial SaaS use). Confirm with legal counsel before
substituting.

## Alternative fonts

Other permissively licensed CJK TTF fonts that may be used as drop-ins
(rename to `ipaexg.ttf` after verifying license compliance):

| Font              | License       | Source                               |
|-------------------|---------------|--------------------------------------|
| Noto Sans JP      | SIL OFL 1.1   | <https://fonts.google.com/noto>      |
| IPAex Mincho      | IPA Font Lic. | <https://moji.or.jp/ipafont/>        |
| Source Han Sans   | SIL OFL 1.1   | <https://github.com/adobe-fonts>     |

## Fallback behaviour

If `ipaexg.ttf` is not present in this directory at program startup,
`pdf.go` logs a notice and uses Helvetica. All Japanese text is then
rendered in romanised / ASCII form. This fallback ensures the application
and its tests remain functional without the font binary.
