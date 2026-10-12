// This directory is a pnpm workspace, not Go code. A nested go.mod makes it its
// own module, so the forgectl module's `./...` patterns never walk node_modules.
module forgectl.invalid/integrations/paperclip

go 1.25
