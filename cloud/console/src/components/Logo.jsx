import { Component, createElement, isValidElement } from "react";
import { useOptions } from "../slots.js";

/** Logo is the branding's logo: an image URL, a React node, or μ. */
export default function Logo({ big, title }) {
	const { logo } = useOptions().branding;
	return <div className={big ? "logo big" : "logo"} title={title}>{logo == null ? "μ" : typeof logo === "string" ? <img src={logo} alt="" /> : logo}</div>;
}

/** slotIcon renders an extension's icon: an element, a component such as a lucide icon, or text. */
export function slotIcon(icon, size, fallback) {
	if (icon == null) return fallback;
	if (isValidElement(icon) || typeof icon === "string") return icon;
	return createElement(icon, { size });
}

/** SlotBoundary keeps an extension's error inside its own slot. */
export class SlotBoundary extends Component {
	state = { error: null };
	static getDerivedStateFromError(error) {
		return { error };
	}
	render() {
		return this.state.error ? <p className="err small pad">{this.props.name}: {String(this.state.error.message ?? this.state.error)}</p> : this.props.children;
	}
}
