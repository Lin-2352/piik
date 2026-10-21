import { useId, useState } from "react";
import { MAX_HOST_PRESET_NAME, presetName } from "../../lib/host-settings";
import { useCopy } from "../../ui/copy";
import { Glyph } from "../../ui/icons";
import { Btn } from "./primitives";

// Presentation only: the Host applies, stores and resets the settings.
export function SavedSettings({ presets, disabled, onApply, onSave, onDelete, onReset }: {
  presets: readonly string[]; disabled?: boolean;
  onApply: (name: string) => void; onSave: (name: string) => void;
  onDelete: (name: string) => void; onReset: () => void;
}) {
  const { t } = useCopy();
  const id = useId();
  const [selected, setSelected] = useState("");
  const [draft, setDraft] = useState("");
  const current = presets.includes(selected) ? selected : "";
  const name = presetName(draft);
  return <div className="lr-capture-device lr-saved-settings">
    <label htmlFor={id}>{t("host.saved.title")}</label>
    <div className="lr-capture-device-controls">
      <select id={id} value={current} disabled={disabled || presets.length === 0}
        aria-label={t("host.saved.choose")}
        onChange={event => {
          setSelected(event.target.value);
          if (event.target.value) onApply(event.target.value);
        }}>
        <option value="">{presets.length ? t("host.saved.choose") : t("host.saved.none")}</option>
        {presets.map(preset => <option key={preset} value={preset}>{preset}</option>)}
      </select>
      <Btn icon="x" title="host.saved.delete" hint="hint-close" disabled={disabled || !current}
        onClick={() => { onDelete(current); setSelected(""); }} />
    </div>
    <div className="lr-capture-device-controls">
      <label className="lr-input">
        <Glyph name="save" size={16} />
        <input type="text" value={draft} maxLength={MAX_HOST_PRESET_NAME} disabled={disabled}
          placeholder={t("host.saved.namePlaceholder")} aria-label={t("host.saved.name")}
          onChange={event => setDraft(event.target.value)}
          onKeyDown={event => {
            if (event.key === "Enter" && name) { onSave(name); setSelected(name); setDraft(""); }
          }} />
      </label>
      <Btn icon="save" title="host.saved.save" hint="hint-advanced" disabled={disabled || !name}
        onClick={() => { if (name) { onSave(name); setSelected(name); setDraft(""); } }} />
      <Btn icon="refresh" title="host.saved.reset" hint="hint-advanced" disabled={disabled}
        onClick={() => { onReset(); setSelected(""); }} />
    </div>
  </div>;
}
