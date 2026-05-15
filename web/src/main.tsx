import { createRoot } from "react-dom/client";
import { App } from "./App";
// Tailwind base + design tokens load first; the existing app stylesheet
// follows so current component styling wins during the coexistence period.
import "./styles/globals.css";
import "./styles/global.css";

// No StrictMode yet: it double-invokes effects, which would change behavior
// while the module-scope query lifecycle still has known issues. Enabled
// after those are addressed.
createRoot(document.getElementById("app")!).render(<App />);
