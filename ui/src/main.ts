import { mount } from "svelte";
import App from "./App.svelte";
import { session } from "./lib/session.svelte";
import "./app.css";

mount(App, { target: document.getElementById("app")! });
session.start();
