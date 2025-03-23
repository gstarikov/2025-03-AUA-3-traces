document.addEventListener("DOMContentLoaded", () => {
    const tableBody = document.querySelector("#items-table tbody");
    const addButton = document.getElementById("add-button");
    const itemNameInput = document.getElementById("item-name");
    const notification = document.getElementById("notification");

    function generateRequestID() {
        return ([1e7]+-1e3+-4e3+-8e3+-1e11).replace(/[018]/g, c =>
            (c ^ crypto.getRandomValues(new Uint8Array(1))[0] & 15 >> c / 4).toString(16)
        );
    }

    function loadItems() {
        fetch("/api/items", {
            headers: {
                "X-Request-ID": generateRequestID()
            }
        })
            .then(res => res.json())
            .then(items => {
                tableBody.innerHTML = "";
                items.forEach(item => {
                    const row = document.createElement("tr");
                    row.innerHTML = `<td>${item.pk}</td><td>${item.data}</td>`;
                    tableBody.appendChild(row);
                });
            })
            .catch(err => console.error("Failed to load items:", err));
    }

    addButton.addEventListener("click", () => {
        const data = itemNameInput.value.trim();
        if (!data) return;

        fetch("/api/items", {
            method: "POST",
            headers: {
                "Content-Type": "application/json",
                "X-Request-ID": generateRequestID()
            },
            body: JSON.stringify({ data }),
        })
            .then(res => res.json())
            .then(item => {
                itemNameInput.value = "";
                notification.textContent = `New item created with ID: ${item.pk}`;
                loadItems();
            })
            .catch(err => console.error("Failed to add item:", err));
    });

    loadItems();
});
