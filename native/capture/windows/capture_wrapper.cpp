#include <windows.h>
#include <string>
#include <vector>

int main() {
    // 1. Attach to user's interactive desktop if available
    HDESK input_desk = OpenInputDesktop(0, FALSE, GENERIC_ALL);
    if (input_desk) {
        SetThreadDesktop(input_desk);
    }

    // 2. Locate piik-capture-bin.exe in the same directory as this wrapper
    wchar_t module_path[MAX_PATH] = {0};
    GetModuleFileNameW(NULL, module_path, MAX_PATH);
    std::wstring path(module_path);
    size_t last_slash = path.find_last_of(L"\\/");
    std::wstring dir = (last_slash != std::wstring::npos) ? path.substr(0, last_slash + 1) : L"";
    std::wstring target_bin = dir + L"piik-capture-bin.exe";

    // 3. Obtain full Unicode command line
    LPCWSTR original_cmd = GetCommandLineW();
    std::vector<wchar_t> cmd_buffer;
    if (original_cmd) {
        cmd_buffer.assign(original_cmd, original_cmd + wcslen(original_cmd) + 1);
    }

    // 4. Set up STARTUPINFO with lpDesktop and standard handles
    STARTUPINFOW si = { sizeof(si) };
    si.lpDesktop = (LPWSTR)L"WinSta0\\Default";
    si.dwFlags = STARTF_USESTDHANDLES;
    si.hStdInput = GetStdHandle(STD_INPUT_HANDLE);
    si.hStdOutput = GetStdHandle(STD_OUTPUT_HANDLE);
    si.hStdError = GetStdHandle(STD_ERROR_HANDLE);

    PROCESS_INFORMATION pi = { 0 };

    BOOL success = CreateProcessW(
        target_bin.c_str(),
        cmd_buffer.empty() ? NULL : cmd_buffer.data(),
        NULL,
        NULL,
        TRUE, // Inherit handles
        0,
        NULL,
        NULL,
        &si,
        &pi
    );

    if (!success) {
        DWORD err = GetLastError();
        if (input_desk) CloseDesktop(input_desk);
        return static_cast<int>(err ? err : 1);
    }

    WaitForSingleObject(pi.hProcess, INFINITE);
    DWORD exit_code = 0;
    GetExitCodeProcess(pi.hProcess, &exit_code);

    CloseHandle(pi.hProcess);
    CloseHandle(pi.hThread);
    if (input_desk) CloseDesktop(input_desk);

    return static_cast<int>(exit_code);
}
