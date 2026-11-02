// Privacy permission checks for the app macOS holds responsible for this
// process (the terminal or IDE that runs mixtape).
#ifndef MIXTAPE_PERM_H
#define MIXTAPE_PERM_H

// States: 0 granted, 1 denied, 2 undecided, -1 unknown.
int mt_perm_system_audio(void);
int mt_perm_microphone(void);
// mt_perm_request_microphone shows the microphone prompt when undecided and
// waits for the answer (up to timeout_seconds).
int mt_perm_request_microphone(int timeout_seconds);
// mt_perm_automation asks whether Apple Events to bundle_id are allowed;
// ask shows the prompt when undecided. 3 means the target is not running.
int mt_perm_automation(const char *bundle_id, int ask);
// mt_responsible_path writes the executable macOS holds responsible for this
// process into buf; it returns 0 on success.
int mt_responsible_path(char *buf, int size);

#endif
