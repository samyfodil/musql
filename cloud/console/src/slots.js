import { createContext, useContext } from "react";
import { normalize } from "./options.js";

/** The options createConsole was given, with their defaults. */
export const Options = createContext(normalize());
export const useOptions = () => useContext(Options);

/** What an extension's page, tab or banner sees; useConsole reads it from any component under one. */
export const Slot = createContext(null);
export const useConsole = () => useContext(Slot);
