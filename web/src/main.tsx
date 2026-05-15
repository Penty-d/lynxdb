import { createRoot } from "react-dom/client";
import { App } from "./App";
import "./styles/globals.css";

// No StrictMode yet: it double-invokes effects, which would change behavior
// while the module-scope query lifecycle still has known issues. Enabled
// after those are addressed.
createRoot(document.getElementById("app")!).render(<App />);
