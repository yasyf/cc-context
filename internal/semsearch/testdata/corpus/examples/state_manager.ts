import { StateManager } from "../typescript/state_manager";

const manager = new StateManager({sessionToken: "demo", activePanel: "search"});
manager.restoreSessionToken({sessionToken: "next", activePanel: "results"});
