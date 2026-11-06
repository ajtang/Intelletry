import next from "eslint-config-next/core-web-vitals";
import typescript from "eslint-config-next/typescript";

const config = [{ ignores: [".next/**", "next-env.d.ts"] }, ...next, ...typescript];

export default config;
