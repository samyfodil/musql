// The package's entry: everything else under src/ is private.

// The stock console, and the context its slots get.
export { createConsole } from "./console.jsx";
export { useConsole } from "./slots.js";

// The pieces it is built from, for an app that is its own shell.
export { ConsoleProvider } from "./provider.jsx";
export { useCloud } from "./cloud/useCloud.js";
export { useTheme } from "./theme.jsx";
export { DatabaseProvider, DatabaseWorkspace, Diagram, QueryView, useDatabase } from "./workspace/DatabaseWorkspace.jsx";
export { default as TableView } from "./components/TableView.jsx";
export { default as Grid } from "./components/Grid.jsx";
export { CLOUD_PAGES, Databases, Groups, Members, OrgSettings, PlatformTokens, Usage, Users } from "./cloud/pages.jsx";

// Building blocks.
export { Toasts, useToast } from "./components/Toasts.jsx";
export { default as Drawer } from "./components/Drawer.jsx";
export { MenuButton, MenuList, useContextMenu } from "./components/Menu.jsx";
export { ImportDialog, TableDialog } from "./components/Dialogs.jsx";
export { default as Palette } from "./components/Palette.jsx";
export { Spinner } from "./components/Busy.jsx";
export { Field } from "./components/ColumnPanel.jsx";
