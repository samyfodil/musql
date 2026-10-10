import { createContext, useContext } from "react";

/** What every view shares: the schema, row counts, and the status line. */
export const Studio = createContext(null);
export const useStudio = () => useContext(Studio);
