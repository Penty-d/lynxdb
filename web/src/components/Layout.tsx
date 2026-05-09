import { ComponentChildren } from "preact";
import { useRouter } from "preact-router";
import { LogoutButton } from "./AuthGate";
import { uiPath } from "../utils/base";
import styles from "./Layout.module.css";

interface Props {
  children: ComponentChildren;
}

export function Layout({ children }: Props) {
  const [routerState] = useRouter();
  const url = routerState?.url ?? "/";

  return (
    <div class={styles.layout}>
      <header class={styles.topbar}>
        <a href={uiPath("/")} class={styles.logo}>
          <img src={uiPath("/lynxdb-icon.png")} alt="LynxDB" class={styles.logoImg} />
          <span class={styles.logoText}>LynxDB</span>
        </a>
        <nav class={styles.navLinks}>
          <a href={uiPath("/")} class={url === uiPath("/") ? styles.active : undefined}>
            Search
          </a>
          <a
            href={uiPath("/status")}
            class={url === uiPath("/status") ? styles.active : undefined}
          >
            Status
          </a>
          <LogoutButton />
        </nav>
      </header>
      <main class={styles.content}>{children}</main>
    </div>
  );
}
